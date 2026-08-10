package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go-llm-proxy/internal/config"
)

// MinerU cloud API endpoints (v4, documented at https://mineru.net/apiManage/docs).
const (
	mineruBaseURL    = "https://mineru.net"
	mineruSubmitPath = "/api/v4/file-urls/batch"
	mineruPollPath   = "/api/v4/extract-results/batch/"
)

// mineruPollInterval is the delay between extraction-status polls.
const mineruPollInterval = 5 * time.Second

// mineruDefaultTimeout bounds the whole extraction (submit → upload → poll →
// download). Overridable via processors.mineru_timeout_sec.
const mineruDefaultTimeout = 300 * time.Second

// mineruDefaultDownloadRetries is how many times the result-zip download is
// retried on transient network errors when the operator hasn't configured
// processors.mineru_download_retries.
const mineruDefaultDownloadRetries = 1

// mineruRetryDelay is the pause between download retry attempts.
const mineruRetryDelay = 1 * time.Second

// mineruBreaker opens after a run of consecutive MinerU failures and
// short-circuits PDF processing during a cloud outage. Without it, every PDF
// request during an outage would spin up a full upload + poll that only fails
// after the whole timeout. Reuses the vision breaker structure (same
// threshold/cooldown semantics).
var mineruBreaker = newVisionBreaker()

// ResetMineruBreaker clears the MinerU circuit breaker. Exported for tests.
func ResetMineruBreaker() {
	mineruBreaker.mu.Lock()
	defer mineruBreaker.mu.Unlock()
	mineruBreaker.failures = 0
	mineruBreaker.openUntil = time.Time{}
}

// mineruResult carries an extracted document. markdown is full.md with images
// still referenced as ![](images/<file>); files is the entire decompressed
// result zip (keyed by path) so the Pipeline layer can read the cropped images
// and content_list_v2.json to turn those references into text descriptions.
type mineruResult struct {
	markdown string
	files    map[string][]byte
}

// MinerUClient wraps the mineru.net cloud API. It is responsible only for the
// "PDF → markdown" extraction — describing the referenced images is the
// Pipeline's job (see processPDFs/describeMineruImages), so this type never
// calls the vision model.
type MinerUClient struct {
	baseURL         string        // API root, e.g. https://mineru.net (tests override)
	token           string        // Bearer token (from processors.mineru_api_key)
	modelVersion    string        // "pipeline" (default) or "vlm"
	timeout         time.Duration // overall extraction timeout
	client          *http.Client
	downloadRetries int // download retry count on transient network errors
}

// NewMinerUClient builds a client from the processors config. client may be nil
// (uses http.DefaultClient). The baseURL defaults to the live MinerU API; tests
// in this package override it to point at a mock server.
func NewMinerUClient(cfg config.ProcessorsConfig, client *http.Client) *MinerUClient {
	timeout := mineruDefaultTimeout
	if cfg.MineruTimeoutSec > 0 {
		timeout = time.Duration(cfg.MineruTimeoutSec) * time.Second
	}
	version := cfg.MineruModelVersion
	if version == "" {
		version = "pipeline"
	}
	retries := cfg.MineruDownloadRetries
	if retries == 0 {
		retries = mineruDefaultDownloadRetries
	}
	hc := client
	if hc == nil {
		hc = http.DefaultClient
	}
	return &MinerUClient{
		baseURL:         mineruBaseURL,
		token:           cfg.MineruAPIKey,
		modelVersion:    version,
		timeout:         timeout,
		client:          hc,
		downloadRetries: retries,
	}
}

// ProcessPDFBytes runs the full MinerU cloud flow and returns the extracted
// markdown plus the decompressed result files. On failure it records the trip
// on the circuit breaker; when the breaker is open it returns immediately
// instead of hitting the API.
func (mc *MinerUClient) ProcessPDFBytes(ctx context.Context, pdfBytes []byte) (*mineruResult, error) {
	if len(pdfBytes) == 0 {
		return nil, fmt.Errorf("empty PDF bytes")
	}
	if mc.token == "" {
		return nil, fmt.Errorf("mineru_api_key not configured")
	}
	if !mineruBreaker.Allow() {
		return nil, fmt.Errorf("MinerU temporarily unavailable (circuit breaker open)")
	}

	// Bound the whole extraction with the configured timeout. The caller's
	// context is tied to the client connection; a long extraction shouldn't be
	// cut off the instant the client disconnects, but shouldn't run forever
	// either (matches describeImage's dedicated-timeout pattern).
	extractCtx, cancel := context.WithTimeout(ctx, mc.timeout)
	defer cancel()

	ok := false
	defer func() {
		if ok {
			mineruBreaker.Success()
		} else {
			mineruBreaker.Failure()
		}
	}()

	batchID, fileURL, err := mc.submitUpload(extractCtx)
	if err != nil {
		return nil, err
	}
	if err := mc.uploadFile(extractCtx, fileURL, pdfBytes); err != nil {
		return nil, err
	}
	fullZipURL, err := mc.pollResult(extractCtx, batchID)
	if err != nil {
		return nil, err
	}
	result, err := mc.downloadAndExtractZip(extractCtx, fullZipURL)
	if err != nil {
		return nil, err
	}

	ok = true
	return result, nil
}

// submitUpload requests a signed upload URL. Returns the batch_id and the first
// file's signed URL.
func (mc *MinerUClient) submitUpload(ctx context.Context) (batchID, fileURL string, err error) {
	body := map[string]any{
		// data_id is intentionally omitted: it's a client dedup key, and a
		// fixed value would make MinerU return a cached result for a *different*
		// PDF submitted under the same id.
		"files":         []any{map[string]any{"name": "document.pdf"}},
		"model_version": mc.modelVersion,
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", mc.baseURL+mineruSubmitPath, bytes.NewReader(payload))
	if err != nil {
		return "", "", fmt.Errorf("build submit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+mc.token)
	req.Header.Set("Accept", "*/*")

	resp, err := mc.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("mineru submit: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("mineru submit: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		Code int `json:"code"`
		Data struct {
			BatchID  string   `json:"batch_id"`
			FileURLs []string `json:"file_urls"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", "", fmt.Errorf("mineru submit: parse response: %w", err)
	}
	if parsed.Code != 0 {
		return "", "", fmt.Errorf("mineru submit: code %d", parsed.Code)
	}
	if parsed.Data.BatchID == "" || len(parsed.Data.FileURLs) == 0 {
		return "", "", fmt.Errorf("mineru submit: missing batch_id or file_urls")
	}
	return parsed.Data.BatchID, parsed.Data.FileURLs[0], nil
}

// uploadFile PUTs the PDF bytes to the signed URL. Deliberately sends no
// Content-Type header — the signed upload URL rejects requests that set one
// (verified against the live API).
func (mc *MinerUClient) uploadFile(ctx context.Context, fileURL string, pdfBytes []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", fileURL, bytes.NewReader(pdfBytes))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	resp, err := mc.client.Do(req)
	if err != nil {
		return fmt.Errorf("mineru upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mineru upload: HTTP %d", resp.StatusCode)
	}
	return nil
}

// pollResult polls the extraction status until a terminal state. Returns the
// full_zip_url on success.
func (mc *MinerUClient) pollResult(ctx context.Context, batchID string) (string, error) {
	pollURL := mc.baseURL + mineruPollPath + batchID
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
		if err != nil {
			return "", fmt.Errorf("build poll request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+mc.token)
		req.Header.Set("Accept", "*/*")

		resp, err := mc.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("mineru poll: %w", err)
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("mineru poll: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		var parsed struct {
			Code int `json:"code"`
			Data struct {
				ExtractResult []struct {
					FileName   string `json:"file_name"`
					State      string `json:"state"`
					ErrMsg     string `json:"err_msg"`
					FullZipURL string `json:"full_zip_url"`
				} `json:"extract_result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return "", fmt.Errorf("mineru poll: parse response: %w", err)
		}
		if parsed.Code != 0 {
			return "", fmt.Errorf("mineru poll: code %d", parsed.Code)
		}

		for _, r := range parsed.Data.ExtractResult {
			switch r.State {
			case "done":
				if r.FullZipURL == "" {
					return "", fmt.Errorf("mineru poll: state done but no full_zip_url")
				}
				return r.FullZipURL, nil
			case "failed":
				return "", fmt.Errorf("mineru extract failed: %s", r.ErrMsg)
			}
		}

		// Neither terminal state yet — wait and poll again. The overall timeout
		// fires via extractCtx, so this only needs to respect the context.
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("mineru poll: %w", ctx.Err())
		case <-time.After(mineruPollInterval):
		}
	}
}

// downloadAndExtractZip downloads the result zip and decompresses it into
// memory. Returns the full.md content plus every extracted file keyed by path.
//
// The download step is retried on transient network errors (connection/TLS
// failure, non-2xx 5xx) up to downloadRetries times. HTTP 4xx (e.g. an expired
// signed URL) is not retried — the URL won't become valid on a second attempt.
func (mc *MinerUClient) downloadAndExtractZip(ctx context.Context, url string) (*mineruResult, error) {
	zipBytes, err := mc.downloadZipWithRetry(ctx, url)
	if err != nil {
		return nil, err
	}

	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("mineru download: open zip: %w", err)
	}

	files := make(map[string][]byte, len(zr.File))
	var markdown string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("mineru download: open %s: %w", f.Name, err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, 64<<20)) // 64 MB per file cap
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("mineru download: read %s: %w", f.Name, err)
		}
		files[f.Name] = data
		if f.Name == "full.md" {
			markdown = string(data)
		}
	}
	if markdown == "" {
		return nil, fmt.Errorf("mineru result zip missing full.md")
	}
	slog.Info("MinerU extraction complete",
		"files", len(files), "markdown_len", len(markdown))
	return &mineruResult{markdown: markdown, files: files}, nil
}

// downloadZipWithRetry GETs the result zip, retrying on transient network
// errors. A retryable failure is either a transport error (connection refused,
// TLS handshake, read timeout) or an HTTP 5xx. 4xx and 3xx are returned
// immediately. Each retry sleeps mineruRetryDelay; the overall mineruTimeout
// (extractCtx) still bounds the whole attempt sequence.
func (mc *MinerUClient) downloadZipWithRetry(ctx context.Context, url string) ([]byte, error) {
	attempts := mc.downloadRetries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			slog.Warn("MinerU zip download failed, retrying",
				"attempt", attempt, "max_attempts", attempts, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("mineru download: %w", ctx.Err())
			case <-time.After(mineruRetryDelay):
			}
		}
		zipBytes, err := mc.downloadZipOnce(ctx, url)
		if err == nil {
			return zipBytes, nil
		}
		lastErr = err
		if !isTransientHTTPError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("mineru download: %w (after %d attempts)", lastErr, attempts)
}

// downloadZipOnce performs a single GET of the result zip and returns the raw
// bytes. The error carries a marker for retryable (transient) failures.
func (mc *MinerUClient) downloadZipOnce(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := mc.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mineru download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("mineru download: HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 500 {
			return nil, transientError{err}
		}
		return nil, err
	}
	zipBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20)) // 512 MB cap
	if err != nil {
		return nil, transientError{fmt.Errorf("mineru download: read: %w", err)}
	}
	return zipBytes, nil
}

// transientError marks an error as retryable (transient network/HTTP 5xx).
type transientError struct {
	err error
}

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() error { return e.err }

// isTransientHTTPError reports whether err is a transient failure worth
// retrying: a transport-level error (connection/TLS/read) or an HTTP 5xx.
// HTTP 4xx (bad request, expired URL) is not retried — the URL won't fix
// itself on a second attempt.
func isTransientHTTPError(err error) bool {
	var te transientError
	return errors.As(err, &te)
}
