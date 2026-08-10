package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go-llm-proxy/internal/config"
)

// buildMineruResultZip constructs a result zip shaped like MinerU's output:
// full.md plus images/ and a *_content_list_v2.json.
func buildMineruResultZip(t *testing.T, fullMD string, images map[string][]byte, contentListV2 string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeFile := func(name, content string) {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, content); err != nil {
			t.Fatal(err)
		}
	}
	if fullMD != "" {
		writeFile("full.md", fullMD)
	}
	for name, data := range images {
		fw, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if contentListV2 != "" {
		writeFile("aaa_content_list_v2.json", contentListV2)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mineruMock simulates the MinerU cloud API (submit → upload → poll → download).
type mineruMock struct {
	server       *httptest.Server
	pollState    string // "done" (default) or "pending"
	pollCount    int
	uploads      []*http.Request
	submitBodies []json.RawMessage
	resultZip    []byte
	// downloadFails is the number of consecutive /result.zip responses to
	// fail with HTTP 500 before succeeding. Used to exercise download retry.
	downloadFails int
	downloadCount int
}

// newMineruMock starts a mock server. When pollState is "done" the extraction
// succeeds on the first poll; "pending" keeps returning pending forever (for
// timeout tests).
func newMineruMock(t *testing.T, resultZip []byte) *mineruMock {
	t.Helper()
	m := &mineruMock{pollState: "done", resultZip: resultZip}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v4/file-urls/batch":
			body, _ := io.ReadAll(r.Body)
			m.submitBodies = append(m.submitBodies, body)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"batch_id":  "test-batch",
					"file_urls": []string{m.server.URL + "/upload/document.pdf"},
				},
			})
		case r.Method == "PUT" && r.URL.Path == "/upload/document.pdf":
			m.uploads = append(m.uploads, r)
			w.WriteHeader(http.StatusOK)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v4/extract-results/batch/"):
			m.pollCount++
			if m.pollState == "pending" {
				json.NewEncoder(w).Encode(map[string]any{
					"code": 0,
					"data": map[string]any{"extract_result": []map[string]any{
						{"file_name": "document.pdf", "state": "pending", "err_msg": "", "full_zip_url": ""},
					}},
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"extract_result": []map[string]any{
					{"file_name": "document.pdf", "state": "done", "err_msg": "", "full_zip_url": m.server.URL + "/result.zip"},
				}},
			})
		case r.Method == "GET" && r.URL.Path == "/result.zip":
			m.downloadCount++
			if m.downloadCount <= m.downloadFails {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Write(m.resultZip)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

// --- MinerUClient unit tests ---

func TestMinerUClient_ProcessPDFBytes_Success(t *testing.T) {
	ResetMineruBreaker()

	fullMD := "# Test\n\n![](images/abc123.jpg)\n"
	img := []byte("fake-jpeg-bytes")
	zipData := buildMineruResultZip(t, fullMD, map[string][]byte{"images/abc123.jpg": img}, "")
	mock := newMineruMock(t, zipData)

	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token", MineruModelVersion: "pipeline"}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	res, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.markdown != fullMD {
		t.Errorf("markdown mismatch:\n got: %q\nwant: %q", res.markdown, fullMD)
	}
	got, ok := res.files["images/abc123.jpg"]
	if !ok {
		t.Fatal("missing image file in result")
	}
	if !bytes.Equal(got, img) {
		t.Error("image bytes mismatch")
	}
	if mock.pollCount == 0 {
		t.Error("expected poll to be called")
	}
}

func TestMinerUClient_Upload_NoContentType(t *testing.T) {
	ResetMineruBreaker()
	mock := newMineruMock(t, buildMineruResultZip(t, "ok", nil, ""))

	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token"}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	if _, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.uploads) != 1 {
		t.Fatalf("expected 1 upload, got %d", len(mock.uploads))
	}
	if ct := mock.uploads[0].Header.Get("Content-Type"); ct != "" {
		t.Errorf("upload must not send Content-Type, got %q", ct)
	}
}

func TestMinerUClient_Poll_Timeout(t *testing.T) {
	ResetMineruBreaker()
	mock := newMineruMock(t, buildMineruResultZip(t, "ok", nil, ""))
	mock.pollState = "pending"

	// 1s timeout so the test doesn't wait the default 300s.
	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token", MineruTimeoutSec: 1}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	_, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "poll") {
		t.Errorf("expected poll error, got: %v", err)
	}
	if mock.pollCount == 0 {
		t.Error("expected at least one poll")
	}
}

func TestMinerUClient_CircuitBreakerShortCircuits(t *testing.T) {
	ResetMineruBreaker()
	mock := newMineruMock(t, buildMineruResultZip(t, "ok", nil, ""))

	// Trip the breaker past the threshold.
	for i := 0; i < visionBreakerThreshold; i++ {
		mineruBreaker.Failure()
	}

	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token"}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	_, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err == nil || !strings.Contains(err.Error(), "circuit breaker") {
		t.Fatalf("expected circuit-breaker short-circuit, got: %v", err)
	}
	if len(mock.uploads) != 0 {
		t.Errorf("breaker open should prevent any API calls, got %d uploads", len(mock.uploads))
	}
}

// TestMinerUClient_DownloadRetry verifies the result-zip download is retried on
// transient failures (HTTP 5xx) and that the extraction still succeeds.
func TestMinerUClient_DownloadRetry(t *testing.T) {
	ResetMineruBreaker()

	zipData := buildMineruResultZip(t, "retried ok", nil, "")
	mock := newMineruMock(t, zipData)
	mock.downloadFails = 1 // first download attempt fails with 500, second succeeds

	// Default retries = 1, so the single 500 is retried and succeeds.
	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token"}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	res, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err != nil {
		t.Fatalf("expected retry to recover, got: %v", err)
	}
	if res.markdown != "retried ok" {
		t.Errorf("markdown mismatch after retry, got %q", res.markdown)
	}
	if mock.downloadCount != 2 {
		t.Errorf("expected 2 download attempts (1 fail + 1 retry), got %d", mock.downloadCount)
	}
}

// TestMinerUClient_DownloadRetryExhausted verifies that when every download
// attempt fails, the extraction returns an error naming the attempt count.
func TestMinerUClient_DownloadRetryExhausted(t *testing.T) {
	ResetMineruBreaker()

	mock := newMineruMock(t, buildMineruResultZip(t, "never", nil, ""))
	mock.downloadFails = 99 // always fail

	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token"}, http.DefaultClient)
	mc.baseURL = mock.server.URL

	_, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err == nil {
		t.Fatal("expected error after exhausting download retries")
	}
	if !strings.Contains(err.Error(), "after 2 attempts") {
		t.Errorf("expected attempt count in error, got: %v", err)
	}
	// The failed download should trip the breaker.
	mineruBreaker.mu.Lock()
	failures := mineruBreaker.failures
	mineruBreaker.mu.Unlock()
	if failures == 0 {
		t.Error("expected mineru breaker to record the failure")
	}
}

// TestMinerUClient_DownloadNoRetryOn4xx verifies HTTP 4xx (e.g. expired signed
// URL) is not retried — a 404/403 won't become valid on a second attempt.
func TestMinerUClient_DownloadNoRetryOn4xx(t *testing.T) {
	ResetMineruBreaker()

	var downloadCount int
	var mock *httptest.Server
	mock = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v4/file-urls/batch":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"batch_id":  "test-batch",
					"file_urls": []string{mock.URL + "/upload/document.pdf"},
				},
			})
		case r.Method == "PUT" && r.URL.Path == "/upload/document.pdf":
			w.WriteHeader(http.StatusOK)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v4/extract-results/batch/"):
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"extract_result": []map[string]any{
					{"file_name": "document.pdf", "state": "done", "err_msg": "", "full_zip_url": mock.URL + "/result.zip"},
				}},
			})
		case r.Method == "GET" && r.URL.Path == "/result.zip":
			downloadCount++
			w.WriteHeader(http.StatusNotFound) // 4xx — not retried
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()

	mc := NewMinerUClient(config.ProcessorsConfig{MineruAPIKey: "test-token"}, http.DefaultClient)
	mc.baseURL = mock.URL

	_, err := mc.ProcessPDFBytes(context.Background(), []byte("%PDF-1.0 fake"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got: %v", err)
	}
	if downloadCount != 1 {
		t.Errorf("4xx should not be retried, expected 1 download attempt, got %d", downloadCount)
	}
}

// --- image caption parsing ---

func TestMinerUImageCaptions(t *testing.T) {
	v2 := `[[
	  {"type":"image","content":{"image_source":{"path":"images/a.jpg"},"image_caption":[{"type":"text","content":"Fig A"}]}},
	  {"type":"chart","content":{"image_source":{"path":"images/b.jpg"},"image_caption":[]}},
	  {"type":"image","content":{"image_source":{"path":"images/c.jpg"},"image_footnote":[{"type":"text","content":"footnote text"}]}},
	  {"type":"paragraph","content":{"paragraph_content":[{"type":"text","content":"not an image"}]}}
	]]`
	files := map[string][]byte{"aaa_content_list_v2.json": []byte(v2)}
	caps := mineruImageCaptions(files)

	if caps["images/a.jpg"] != "Fig A" {
		t.Errorf("caption for a.jpg = %q, want %q", caps["images/a.jpg"], "Fig A")
	}
	if caps["images/b.jpg"] != "" {
		t.Errorf("b.jpg should have no caption, got %q", caps["images/b.jpg"])
	}
	if caps["images/c.jpg"] != "footnote text" {
		t.Errorf("c.jpg should use footnote, got %q", caps["images/c.jpg"])
	}
}

// --- end-to-end processPDFs tests ---

func TestProcessPDFs_MinerUPath(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()
	ResetVisionBreaker()

	// Vision model mock: describes any image it receives.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "v1", "model": "vision-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "A chart showing test data."},
			}},
		})
	}))
	defer visionServer.Close()

	// MinerU mock: extraction succeeds, one image to describe.
	fullMD := "# Test Doc\n\nSome extracted text.\n\n![](images/abc123.jpg)\n\nMore text."
	contentV2 := `[[
	  {"type":"paragraph","content":{"paragraph_content":[{"type":"text","content":"Some extracted text."}]}},
	  {"type":"image","content":{"image_source":{"path":"images/abc123.jpg"},"image_caption":[{"type":"text","content":"Figure 1: BER curves"}]}}
	]]`
	zipData := buildMineruResultZip(t, fullMD,
		map[string][]byte{"images/abc123.jpg": []byte("fakejpeg")}, contentV2)
	mock := newMineruMock(t, zipData)

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{
			Vision:       "vision-model",
			MineruAPIKey: "test-token",
		},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.server.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "pdf_data", "data": b64PDF, "filename": "report.pdf"},
				},
			},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	content := userMsg["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	for _, want := range []string{
		`source="mineru"`,
		"Some extracted text",
		"A chart showing test data", // vision description of the cropped image
		"Figure 1: BER curves",      // MinerU caption prefix
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in result:\n%s", want, text)
		}
	}
	if strings.Contains(text, "![](images/") {
		t.Errorf("image reference was not replaced:\n%s", text)
	}
}

func TestProcessPDFs_MinerU_NoVisionModel(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()

	// MinerU succeeds but no vision model is configured → image refs are
	// stripped, the extracted text is still usable.
	fullMD := "# Test\n\nText body.\n\n![](images/abc123.jpg)\n"
	zipData := buildMineruResultZip(t, fullMD, map[string][]byte{"images/abc123.jpg": []byte("x")}, "")
	mock := newMineruMock(t, zipData)

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{MineruAPIKey: "test-token"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.server.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "pdf_data", "data": b64PDF},
			}},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	content := userMsg["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	if !strings.Contains(text, `source="mineru"`) {
		t.Errorf("expected mineru content, got: %s", text)
	}
	if !strings.Contains(text, "Text body") {
		t.Errorf("expected extracted text, got: %s", text)
	}
	if strings.Contains(text, "![](images/") {
		t.Errorf("image reference should be stripped without a vision model: %s", text)
	}
}

func TestProcessPDFs_MinerUFailure_Placeholder(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()

	// MinerU mock that fails on submit (HTTP 500).
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer mock.Close()

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{MineruAPIKey: "test-token"},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "pdf_data", "data": b64PDF, "filename": "bad.pdf"},
			}},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	content := userMsg["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	if !strings.Contains(text, "[PDF: MinerU processing failed") {
		t.Errorf("expected failure placeholder, got: %s", text)
	}
	if !strings.Contains(text, "HTTP 500") {
		t.Errorf("expected error reason in placeholder, got: %s", text)
	}

	// The breaker should have recorded the failure.
	mineruBreaker.mu.Lock()
	failures := mineruBreaker.failures
	mineruBreaker.mu.Unlock()
	if failures == 0 {
		t.Error("expected mineru breaker to record a failure")
	}
}

// TestProcessPDFs_MinerUImageDescriptionFailure verifies that a per-image
// vision failure degrades gracefully (caption kept, no image description,
// PDF still returned) instead of failing the whole PDF.
func TestProcessPDFs_MinerUImageDescriptionFailure(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()
	ResetVisionBreaker()

	// Vision model that always errors.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"model unavailable"}`))
	}))
	defer visionServer.Close()

	fullMD := "# Test\n\n![](images/abc123.jpg)\n"
	contentV2 := `[[{"type":"image","content":{"image_source":{"path":"images/abc123.jpg"},"image_caption":[{"type":"text","content":"Fig 1"}]}}]]`
	zipData := buildMineruResultZip(t, fullMD,
		map[string][]byte{"images/abc123.jpg": []byte("fakejpeg")}, contentV2)
	mock := newMineruMock(t, zipData)

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{
			Vision:       "vision-model",
			MineruAPIKey: "test-token",
		},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.server.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "pdf_data", "data": b64PDF},
			}},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	content := userMsg["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	if !strings.Contains(text, `source="mineru"`) {
		t.Errorf("expected mineru content despite image failure, got: %s", text)
	}
	if !strings.Contains(text, "Fig 1") {
		t.Errorf("expected caption to be kept, got: %s", text)
	}
	if strings.Contains(text, "![](images/") {
		t.Errorf("image reference should be replaced with caption-only, got: %s", text)
	}
}

// TestMineruFailurePlaceholderFormat pins the placeholder format so operators
// can grep for it reliably.
func TestMineruFailurePlaceholderFormat(t *testing.T) {
	got := mineruFailurePlaceholder(fmt.Errorf("some reason"))
	want := "[PDF: MinerU processing failed — some reason]"
	if got != want {
		t.Errorf("placeholder mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestProcessPDFs_MinerUImagesConcurrent verifies MinerU cropped images are
// described concurrently (bounded by maxConcurrentVision) and that the
// markdown substitution keeps the original order regardless of completion
// order.
func TestProcessPDFs_MinerUImagesConcurrent(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()
	ResetVisionBreaker()

	var mu sync.Mutex
	var maxConcurrent, current int

	// Vision mock: parse the data URL, sleep to widen the overlap window, and
	// return a description unique to each image so we can check ordering.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			current--
			mu.Unlock()
		}()

		// Find which image this is by inspecting the data URL payload.
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		content := req["messages"].([]any)[0].(map[string]any)["content"].([]any)
		var imgURL string
		for _, c := range content {
			m := c.(map[string]any)
			if m["type"] == "image_url" {
				imgURL = m["image_url"].(map[string]any)["url"].(string)
			}
		}
		// data:image/jpeg;base64,<b64 of "img-N">
		b64 := strings.TrimPrefix(imgURL, "data:image/jpeg;base64,")
		raw, _ := base64.StdEncoding.DecodeString(b64)
		desc := "Desc for " + string(raw)

		time.Sleep(100 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "v1", "model": "vision-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": desc},
			}},
		})
	}))
	defer visionServer.Close()

	// 8 unique images in markdown order.
	var fullMD strings.Builder
	fullMD.WriteString("# Doc\n\n")
	images := map[string][]byte{}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("images/img%d.jpg", i)
		fullMD.WriteString(fmt.Sprintf("![](%s)\n\n", name))
		images[name] = []byte(fmt.Sprintf("img-%d", i))
	}
	zipData := buildMineruResultZip(t, fullMD.String(), images, "")
	mock := newMineruMock(t, zipData)

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{
			Vision:       "vision-model",
			MineruAPIKey: "test-token",
		},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.server.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "pdf_data", "data": b64PDF},
			}},
		},
	}

	result, err := p.ProcessRequest(context.Background(), chatReq, model)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := result["messages"].([]any)
	userMsg := msgs[0].(map[string]any)
	text := userMsg["content"].([]any)[0].(map[string]any)["text"].(string)

	if maxConcurrent < 2 {
		t.Fatalf("expected concurrent description, got max in-flight %d", maxConcurrent)
	}
	if strings.Contains(text, "![](images/") {
		t.Errorf("image reference left in output:\n%s", text)
	}

	// Ordering: each "Desc for img-N" must appear before "Desc for img-(N+1)".
	for i := 0; i < 8; i++ {
		want := fmt.Sprintf("Desc for img-%d", i)
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in output", want)
		}
		idx := strings.Index(text, want)
		for j := 0; j < i; j++ {
			prev := strings.Index(text, fmt.Sprintf("Desc for img-%d", j))
			if prev > idx {
				t.Errorf("order violated: img-%d appears before img-%d", i, j)
			}
		}
	}
}

// TestProcessPDFs_MinerUImages_Cancellation verifies that cancelling the parent
// context does not leave the caller stuck waiting for in-flight vision calls
// (describeImage has its own 120s timeout and ignores the parent ctx).
func TestProcessPDFs_MinerUImages_Cancellation(t *testing.T) {
	ResetPDFCache()
	ResetMineruBreaker()
	ResetVisionBreaker()

	release := make(chan struct{})
	// Vision mock blocks until release is closed, guaranteeing in-flight calls
	// never return on their own.
	visionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "v1", "model": "vision-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "late result"},
			}},
		})
	}))
	defer visionServer.Close()

	// A few images so multiple workers are in flight when we cancel.
	fullMD := "# Doc\n\n![](images/a.jpg)\n\n![](images/b.jpg)\n\n![](images/c.jpg)\n"
	zipData := buildMineruResultZip(t, fullMD, map[string][]byte{
		"images/a.jpg": []byte("a"),
		"images/b.jpg": []byte("b"),
		"images/c.jpg": []byte("c"),
	}, "")
	mock := newMineruMock(t, zipData)

	cfg := &config.Config{
		Processors: config.ProcessorsConfig{
			Vision:       "vision-model",
			MineruAPIKey: "test-token",
		},
		Models: []config.ModelConfig{
			{Name: "target", Backend: "http://localhost/v1", Timeout: 30},
			{Name: "vision-model", Backend: visionServer.URL + "/v1", Timeout: 30},
		},
	}
	cs := config.NewTestConfigStore(cfg)
	p := NewPipeline(cs, http.DefaultClient)
	mc := NewMinerUClient(cfg.Processors, http.DefaultClient)
	mc.baseURL = mock.server.URL
	p.mineruClient = mc
	model := &cfg.Models[0]

	b64PDF := base64.StdEncoding.EncodeToString([]byte("%PDF-1.0 fake"))
	chatReq := map[string]any{
		"model": "target",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "pdf_data", "data": b64PDF},
			}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = p.ProcessRequest(ctx, chatReq, model)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond) // let workers reach the blocking server
	cancel()

	// With the escape hatch, ProcessRequest returns well under the 120s vision
	// timeout. Use a 5s bound to keep the test fast.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessRequest did not return after context cancellation")
	}

	// Release the blocked workers so httptest.Server.Close (deferred) can exit.
	close(release)
}
