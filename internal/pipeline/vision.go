package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go-llm-proxy/internal/api"
	"go-llm-proxy/internal/config"
)

// imageCache stores image URL hash → description so that images are only
// processed once. Subsequent requests containing the same image reuse the
// cached description, making follow-up turns fast. Bounded to prevent
// unbounded memory growth in long-running processes.
var imageCache = newBoundedCache()

// ResetImageCache clears the image description cache. Exported for testing.
func ResetImageCache() {
	imageCache.Reset()
}

// visionBreaker is a lightweight circuit breaker that opens after a run of
// consecutive vision/OCR failures and short-circuits processing for a cooldown
// window. When the vision backend is down, every image-carrying request would
// otherwise spin up vision goroutines and wait the full per-call timeout (up to
// 120s) before failing — with max_images_per_request=100 and maxConcurrentVision
// that can stall a request for minutes. Opening the breaker turns all image
// processing into immediate placeholders during the outage, then half-opens
// after the cooldown to let one attempt through and re-close on success.
//
// The breaker is keyed per vision/OCR model name so one flaky backend doesn't
// trip the breaker for a healthy one. It is request-agnostic (a package-level
// singleton), which is deliberate: an outage is a property of the backend, not
// of any single request.
var visionBreaker = newVisionBreaker()

// visionBreakerConfig bounds the circuit breaker. Thresholds are chosen to be
// forgiving of transient hiccups (a single failed image or a brief upstream
// blip shouldn't open the breaker) while still failing fast during a real
// outage (100 failed images at maxConcurrentVision=5 trip it in ~10 batches).
const (
	// visionBreakerThreshold consecutive failures before the breaker opens.
	visionBreakerThreshold = 10
	// visionBreakerCooldown is how long the breaker stays open before the
	// first half-open probe is allowed.
	visionBreakerCooldown = 30 * time.Second
)

// visionBreakerState tracks consecutive vision/OCR failures and cooldown state.
// All fields are guarded by mu.
type visionBreakerState struct {
	mu         sync.Mutex
	failures   int        // consecutive failures since last success
	openUntil  time.Time  // zero = closed; time when the breaker may probe again
	lastAccess time.Time  // for tests to reset
}

// newVisionBreaker returns a fresh, closed breaker.
func newVisionBreaker() *visionBreakerState {
	return &visionBreakerState{}
}

// Allow reports whether a vision call may proceed. On open and still cooling
// down it returns false (short-circuit); on the first attempt after the
// cooldown it half-opens and returns true (allow a probe through).
func (b *visionBreakerState) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures >= visionBreakerThreshold {
		if time.Now().After(b.openUntil) {
			// Cooldown elapsed — half-open: let one probe attempt through.
			b.failures = visionBreakerThreshold - 1 // reserve so the probe counts as the deciding failure
			return true
		}
		return false
	}
	return true
}

// Success records a successful vision call, closing the breaker.
func (b *visionBreakerState) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openUntil = time.Time{}
}

// Failure records a failed vision call. When the consecutive-failure count
// reaches the threshold, the breaker opens and the cooldown starts.
func (b *visionBreakerState) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= visionBreakerThreshold {
		b.openUntil = time.Now().Add(visionBreakerCooldown)
	}
}

// ResetVisionBreaker clears the breaker state. Exported for tests.
func ResetVisionBreaker() {
	visionBreaker.mu.Lock()
	defer visionBreaker.mu.Unlock()
	visionBreaker.failures = 0
	visionBreaker.openUntil = time.Time{}
}

// defaultMaxImagesPerRequest caps the number of unique images the vision
// processor will handle in a single request when the operator hasn't configured
// processors.max_images_per_request. Beyond this, remaining images get a
// placeholder to prevent a single request from triggering unbounded outbound
// HTTP calls.
const defaultMaxImagesPerRequest = 10

// maxConcurrentVision limits how many concurrent vision model calls are made.
const maxConcurrentVision = 5

// imageFailureTTL is how long a failed image extraction is cached. Short
// enough to allow retry after transient upstream issues, long enough to
// prevent re-running the full cascade on every turn for the same image.
const imageFailureTTL = 5 * time.Minute

// minDescriptionLen is the minimum length (in runes) for a *user-role* vision
// description to be accepted and permanently cached. Set deliberately low: real
// descriptions of simple images can be short ("A red circle on white"), so this
// is only a backstop against degenerate fragments — the primary truncation
// signal is finish_reason=length, and stale-model reuse is prevented by the
// model+prompt key. Descriptions shorter than this are not cached permanently,
// so a following turn retries. Tool-role OCR/PDF output is exempt — "No text"
// is a legitimate result there and the cascade handles it.
const minDescriptionLen = 10

// Vision prompts — the describe prompt is for general images; the OCR prompt is
// for PDF page images where text extraction is more useful than visual description.
// The short OCR prompt is for dedicated OCR models (e.g., PaddleOCR-VL) that
// respond to task-specific prefixes.
const (
	visionPromptDescribe = "Describe this image accurately and objectively. Include all visible subjects, objects, text, and relevant details. Be specific about what you observe."
	visionPromptOCR      = "Extract all text from this page. Reproduce the text content verbatim, preserving structure (headings, paragraphs, lists, tables). Focus on text content, not visual layout."
	ocrModelPrompt       = "OCR:"
)

// processImages detects image content in the translated Chat Completions request,
// sends each image to the vision model for description, and replaces the image_url
// parts with text descriptions. Images are processed concurrently for speed, and
// PDF page images (detected via tool result heuristics) use the OCR model with a
// text-extraction prompt. ocrModel may be nil, in which case visionModel is used.
// visionMaxTokens overrides the per-image description caps when non-zero (0 = use
// built-in defaults of 1000 for user images / 2000 for tool images).
func (p *Pipeline) processImages(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig, ocrModel *config.ModelConfig, visionMaxTokens int, maxImagesPerRequest int) (map[string]any, error) {

	if maxImagesPerRequest <= 0 {
		maxImagesPerRequest = defaultMaxImagesPerRequest
	}

	// Normalize messages to []any — translation layers may produce []map[string]any.
	var messages []any
	switch m := chatReq["messages"].(type) {
	case []any:
		messages = m
	case []map[string]any:
		messages = make([]any, len(m))
		for i, msg := range m {
			messages[i] = msg
		}
		chatReq["messages"] = messages
	default:
		return chatReq, nil
	}

	// --- First pass: collect all images that need processing. ---
	//
	// Each image may produce up to two jobs: a vision (describe) job and an
	// OCR (text extraction) job. Cache keys use suffixes ":v" and ":o" to
	// store results independently.
	//
	// For tool-role images (PDF pages, view_image output): OCR only.
	// For user-role images: vision always + OCR if an OCR model is configured.
	type imageJob struct {
		url       string
		cacheKey  string // hash + ":v" or ":o"
		failKey   string // hash + ":fail" — TTL'd sentinel used to short-circuit retries
		prompt    string
		maxTokens int
		model     *config.ModelConfig
		role      string // "user" or "tool" — whether the description came from a user image or tool/PDF output
		// Fallback stage: used by the tool-role OCR→vision cascade. When the
		// primary model fails or returns empty, retry with fallbackModel
		// using fallbackPrompt. Zero-valued when no cascade is needed (user-role
		// images, or deployments with only one processor configured).
		fallbackModel  *config.ModelConfig
		fallbackPrompt string
	}
	var jobs []imageJob
	seenKeys := map[string]bool{}
	// countSeen tracks unique image URLs seen so far in this request so that
	// the maxImagesPerRequest cap counts *unique* images, not raw image_url
	// parts. Claude Code replays the full conversation history on every request,
	// so the same pasted image can appear in many messages; counting each
	// occurrence would exhaust the cap on history alone and silently drop the
	// user's new image. This mirrors the replacement pass (replacementSeen).
	countSeen := map[string]bool{}

	imageCount := 0
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}

		role, _ := msgMap["role"].(string)
		isToolRole := role == "tool"

		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok || partMap["type"] != "image_url" {
				continue
			}

			url := extractImageURL(partMap)
			if url == "" {
				continue
			}

			// Cap on unique images only. Re-references of an image already seen
			// in this request don't consume additional budget — they'll reuse the
			// cached/fresh description in the replacement pass.
			urlHash := hashImageURL(url)
			if countSeen[urlHash] {
				continue
			}
			countSeen[urlHash] = true

			imageCount++
			if imageCount > maxImagesPerRequest {
				slog.Warn("too many unique images in request; skipping image processing",
					"image_count", imageCount-1, "limit", maxImagesPerRequest)
				continue
			}			// SSRF pre-flight: cheaply reject URLs whose hostname itself is
			// an obvious block-target (literal private IP, metadata name).
			// The authoritative enforcement is the safeHTTPClient dialer
			// used later in describeImage — it re-validates every resolved
			// IP at connect time, which closes the DNS-rebinding window.
			if !imageURLPreflight(url) {
				slog.Warn("blocked image URL targeting internal network", "url_prefix", url[:min(len(url), 60)])
				continue
			}

			// Cache key for the current image_url part is computed per-branch
			// below (tool vs user), folding in the processor model and prompt so
			// that switching models or prompts yields a fresh key.
			if isToolRole {
				// Tool-role images (PDF pages, screenshots): OCR → vision cascade.
				// Primary: dedicated OCR model if configured; else vision w/
				// OCR-style prompt (matches pre-cascade behavior).
				primary := visionModel
				primaryPrompt := visionPromptOCR
				if ocrModel != nil {
					primary = ocrModel
					primaryPrompt = ocrModelPrompt
				}
				// Cache key folds image URL + processor model + prompt so that
				// switching models (or prompts) yields a fresh key. Image URL alone
				// is insufficient — the same URL cached a description from a
				// previous model would otherwise be silently reused.
				hash := imageCacheKey(url, true, visionModel, ocrModel)
				ocrKey := hash + ":o"
				failKey := hash + ":fail"
				if _, ok := imageCache.Load(ocrKey); ok {
					continue
				}
				if _, ok := imageCache.Load(failKey); ok {
					// Recent failure still cached — skip until TTL expires.
					continue
				}
				if seenKeys[ocrKey] {
					continue
				}
				seenKeys[ocrKey] = true

				// Fallback: vision model, but only if it's a different instance
				// than the primary. Avoids double-calling the same backend when
				// the operator configured only one processor.
				var fallbackMdl *config.ModelConfig
				fallbackPrompt := ""
				if visionModel != nil && primary != nil && visionModel.Name != primary.Name {
					fallbackMdl = visionModel
					fallbackPrompt = visionPromptOCR
				}
				jobs = append(jobs, imageJob{
					url: url, cacheKey: ocrKey, failKey: failKey,
					prompt: primaryPrompt, maxTokens: maxTokensForRole("tool", visionMaxTokens), model: primary,
					role:           "tool",
					fallbackModel:  fallbackMdl, fallbackPrompt: fallbackPrompt,
				})
			} else {
				// User-role images: vision description only.
				// OCR is skipped for user-attached photos — dedicated OCR models
				// hallucinate on natural images. Text in photos is captured
				// adequately by the vision model's description.
				vKey := imageCacheKey(url, false, visionModel, ocrModel) + ":v"
				if _, ok := imageCache.Load(vKey); !ok && !seenKeys[vKey] {
					seenKeys[vKey] = true
					jobs = append(jobs, imageJob{
						url: url, cacheKey: vKey,
						prompt: visionPromptDescribe, maxTokens: maxTokensForRole("user", visionMaxTokens), model: visionModel,
						role: "user",
					})
				}
			}
		}
	}

	// --- Process all uncached images concurrently. ---
	type jobResult struct {
		desc string
		err  error
	}
	results := make([]jobResult, len(jobs))

	if len(jobs) > 0 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxConcurrentVision)

		for i, job := range jobs {
			wg.Add(1)
			go func(idx int, j imageJob) {
				defer wg.Done()
				select {
				case <-ctx.Done():
					// Parent context cancelled (client disconnect, request
					// timeout) while waiting for a concurrency slot — record
					// the cancellation and exit without calling the vision
					// backend. The wg.Wait below drains on ctx.Done so a
					// cancelled request doesn't block forever waiting on jobs
					// that may be stuck in long upstream calls.
					results[idx] = jobResult{desc: "", err: ctx.Err()}
					return
				case sem <- struct{}{}:
				}
				defer func() { <-sem }()

				// Circuit breaker: if the vision backend is in a failure
				// streak, short-circuit this image to a placeholder instead of
				// spinning up a call that will only time out.
				if !visionBreaker.Allow() {
					results[idx] = jobResult{desc: "", err: fmt.Errorf("vision backend temporarily unavailable (circuit breaker open)")}
					return
				}

				desc, err := p.describeImage(ctx, j.model, j.url, j.prompt, j.maxTokens)
				// Cascade: if the primary attempt failed or came back empty,
				// and a fallback is configured, retry with the fallback.
				if (err != nil || strings.TrimSpace(desc) == "") && j.fallbackModel != nil {
					slog.Warn("image pipeline stage failed, trying fallback",
						"stage", "primary",
						"primary_model", j.model.Name,
						"fallback_model", j.fallbackModel.Name,
						"error", err)
					desc, err = p.describeImage(ctx, j.fallbackModel, j.url, j.fallbackPrompt, j.maxTokens)
					if err == nil && strings.TrimSpace(desc) != "" {
						slog.Info("image pipeline fallback succeeded",
							"fallback_model", j.fallbackModel.Name)
					}
				}
				results[idx] = jobResult{desc: desc, err: err}
			}(i, job)
		}
		// Wait for all jobs, but don't block forever if the parent context was
		// cancelled (client disconnect / request timeout). Workers exit early on
		// ctx.Done when acquiring a slot, but a worker already inside a long
		// vision call may not return promptly — so wait with a ctx.Done escape
		// hatch. A worker that was still running finishes later in the
		// background; recording its result after we've emitted placeholders is
		// harmless because a cancelled request's downstream is gone anyway.
		allDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(allDone)
		}()
		select {
		case <-allDone:
		case <-ctx.Done():
			slog.Warn("vision processing cancelled by context", "error", ctx.Err())
		}

		// Cache successful results permanently; failures short-TTL.
		for i, r := range results {
			if r.err != nil || strings.TrimSpace(r.desc) == "" {
				if r.err != nil {
					slog.Warn("failed to process image",
						"model", jobs[i].model.Name, "cache_key", jobs[i].cacheKey, "error", r.err)
					visionBreaker.Failure()
				}
				// Cache the failure briefly to prevent a cascade re-run on
				// every subsequent turn while the underlying upstream is
				// misbehaving. Only applies when a failKey was set (tool-role).
				if jobs[i].failKey != "" {
					imageCache.StoreWithTTL(jobs[i].failKey, "1", imageFailureTTL)
				}
			} else {
				visionBreaker.Success()
				// A user-role description that is suspiciously short is almost
				// certainly a truncated fragment ("This is a screenshot of a"),
				// not a real description. Do not permanently cache it — treat it
				// as a failure so the next turn retries rather than surfacing
				// (and caching) the fragment forever. Tool-role OCR/PDF output is
				// exempt: "No text" is legitimate there and the cascade handles it.
				if jobs[i].role == "user" && len([]rune(strings.TrimSpace(r.desc))) < minDescriptionLen {
					slog.Warn("vision description too short; not caching",
						"model", jobs[i].model.Name, "cache_key", jobs[i].cacheKey,
						"len", len([]rune(strings.TrimSpace(r.desc))))
					visionBreaker.Failure()
					continue
				}
				imageCache.Store(jobs[i].cacheKey, r.desc)
			}
		}
	}

	// Build a lookup from cache key → result for jobs that just completed.
	jobDescriptions := map[string]string{}
	jobErrors := map[string]bool{}
	for i, r := range results {
		if r.err != nil {
			jobErrors[jobs[i].cacheKey] = true
		} else {
			jobDescriptions[jobs[i].cacheKey] = r.desc
		}
	}

	// --- Second pass: replace images with combined descriptions. ---
	imageCount = 0
	anyModified := false
	// replacementSeen tracks the unique images already emitted in this pass.
	// The replacement counter counts *unique* images, not raw image_url parts —
	// a request that re-references the same image across messages/turns should
	// not consume extra budget for repeats (matching how the first pass dedups
	// processing via seenKeys). Only the first occurrence of each unique image
	// counts toward maxImagesPerRequest.
	replacementSeen := map[string]bool{}
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}

		role, _ := msgMap["role"].(string)
		isToolRole := role == "tool"

		msgModified := false
		newContent := make([]any, 0, len(content))
		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				newContent = append(newContent, part)
				continue
			}
			if partMap["type"] != "image_url" {
				newContent = append(newContent, part)
				continue
			}

			imageURL := extractImageURL(partMap)
			if imageURL == "" {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[Image: unsupported format]",
				})
				msgModified = true
				continue
			}

			// Unique-image de-dup for the max-images cap uses the URL hash alone —
			// "same image re-referenced" is what matters there, not which model
			// described it. The description lookup below uses the full cache key
			// (imageCacheKey), which folds in the model+prompt and MUST match the
			// key used in the processing pass.
			urlHash := hashImageURL(imageURL)

			// Only the first occurrence of each unique image counts toward the cap.
			// Re-references of an already-emitted image reuse its description and
			// do not consume additional budget.
			firstSeen := !replacementSeen[urlHash]
			replacementSeen[urlHash] = true
			if firstSeen {
				imageCount++
				if imageCount > maxImagesPerRequest {
					newContent = append(newContent, map[string]any{
						"type": "text",
						"text": "[Image omitted: too many images in request]",
					})
					msgModified = true
					continue
				}
			}

			// Resolve the same cache key the processing pass used, so we look up
			// the description that was actually stored for this image/model/prompt.
			keyHash := imageCacheKey(imageURL, isToolRole, visionModel, ocrModel)
			replacement := buildImageReplacement(keyHash, isToolRole, imageCache, jobDescriptions)

			newContent = append(newContent, map[string]any{
				"type": "text",
				"text": replacement,
			})
			msgModified = true
		}

		if msgModified {
			msgMap["content"] = newContent
			messages[i] = msgMap
			anyModified = true
		}
	}

	if anyModified {
		chatReq["messages"] = messages
	}
	return chatReq, nil
}

// normalizeContentParts converts a message's content field to []any, handling
// both []any (from messages_translate) and []map[string]any (from responses_translate).
// Returns nil if content is not an array type. If the content was []map[string]any,
// it is also updated in the message map for downstream consistency.
func normalizeContentParts(msgMap map[string]any) []any {
	switch c := msgMap["content"].(type) {
	case []any:
		return c
	case []map[string]any:
		parts := make([]any, len(c))
		for i, p := range c {
			parts[i] = p
		}
		msgMap["content"] = parts
		return parts
	default:
		return nil
	}
}

// buildImageReplacement constructs the replacement text for a single image.
//
// For tool-role images (PDF pages, screenshots): OCR text only.
// For user-role images: vision description only.
//
// Output uses XML-like tags so target models clearly distinguish pipeline-
// injected content from user-authored text. Failures use plain bracketed
// strings — they're empty/informational, wrapping them in tags adds no value.
func buildImageReplacement(hash string, isToolRole bool, cache *boundedCache, jobDescs map[string]string) string {
	// Helper to look up a result from cache or fresh job results.
	lookup := func(cacheKey string) (string, bool) {
		if cached, ok := cache.Load(cacheKey); ok {
			return cached, true
		}
		if desc, ok := jobDescs[cacheKey]; ok {
			return desc, true
		}
		return "", false
	}

	if isToolRole {
		// Tool-role: OCR only.
		if ocrText, ok := lookup(hash + ":o"); ok {
			return fmt.Sprintf("<page_text>%s</page_text>", ocrText)
		}
		slog.Warn("image replacement miss (tool-role)",
			"cache_key", hash+":o", "cache_size", cache.Size())
		return "[Image could not be processed]"
	}

	// User-role: vision description only.
	if visionDesc, ok := lookup(hash + ":v"); ok {
		return fmt.Sprintf("<image_description>%s</image_description>", visionDesc)
	}
	slog.Warn("image replacement miss (user-role)",
		"cache_key", hash+":v", "cache_size", cache.Size())
	return "[Image could not be processed]"
}

// hashImageURL returns a hex-encoded SHA-256 hash of the image URL (or data URL).
// This is used as the cache key for image descriptions.
func hashImageURL(imageURL string) string {
	h := sha256.Sum256([]byte(imageURL))
	return fmt.Sprintf("%x", h)
}

// imageCacheKey computes the cache-key hash for an image, folding the image
// URL together with the processor model and prompt so that switching the
// vision/OCR model (or prompt) produces a fresh key instead of silently
// reusing a stale description cached under the old model. The model/prompt
// resolution mirrors the job-building logic and MUST stay identical between
// the processing pass and the replacement pass, or the two passes will look
// up different keys.
func imageCacheKey(imageURL string, isToolRole bool, visionModel, ocrModel *config.ModelConfig) string {
	if isToolRole {
		primary := visionModel
		primaryPrompt := visionPromptOCR
		if ocrModel != nil {
			primary = ocrModel
			primaryPrompt = ocrModelPrompt
		}
		return hashImageURL(imageURL + "|" + primary.Name + "|" + primaryPrompt)
	}
	return hashImageURL(imageURL + "|" + visionModel.Name + "|" + visionPromptDescribe)
}

// maxTokensForRole returns the description token cap for an image job.
// An explicit non-zero visionMaxTokens (from processors.vision_max_tokens)
// overrides the built-in per-role defaults.
func maxTokensForRole(role string, visionMaxTokens int) int {
	if visionMaxTokens > 0 {
		return visionMaxTokens
	}
	if role == "tool" {
		return 2000
	}
	return 1000
}

// extractImageURL gets the URL string from an image_url content part.
func extractImageURL(part map[string]any) string {
	iu, ok := part["image_url"].(map[string]any)
	if !ok {
		return ""
	}
	u, _ := iu["url"].(string)
	return u
}

// imageURLPreflight returns false for URLs that are obviously unsafe just
// from the raw string — wrong scheme, literal private IP, metadata hostname.
// Deliberately does NOT resolve DNS: the authoritative IP check happens at
// dial time inside safeHTTPClient, which closes the rebinding window.
func imageURLPreflight(imageURL string) bool {
	if strings.HasPrefix(imageURL, "data:") {
		return true
	}
	parsed, err := url.Parse(imageURL)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return preflightURLSafe(parsed)
}

// isPrivateIP returns true if the IP is in a private, loopback, link-local,
// unspecified, or cloud-metadata range that should not be reachable from
// user-supplied URLs. Normalizes IPv4-mapped IPv6 (::ffff:a.b.c.d) to its
// IPv4 form so attackers can't bypass the filter by using the v6-mapped
// encoding of a private v4 address.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// Unspecified (0.0.0.0 / ::) is routed to the local host on most
	// platforms — treat as loopback.
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() {
		return true
	}
	// IsPrivate() covers RFC 1918 and ULA (fc00::/7). Explicit ranges below
	// cover AWS/GCP/Azure metadata endpoints (169.254.169.254 is inside
	// 169.254.0.0/16, which IsLinkLocalUnicast already catches) plus
	// carrier-grade NAT.
	for _, cidr := range extraPrivateCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var extraPrivateCIDRs = func() []*net.IPNet {
	ranges := []string{
		"100.64.0.0/10", // carrier-grade NAT (RFC 6598)
		"::/128",        // unspecified (defense-in-depth; IsUnspecified covers this too)
	}
	out := make([]*net.IPNet, 0, len(ranges))
	for _, cidr := range ranges {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// describeImage sends an image to a vision-capable model and returns a text description.
// The prompt and maxTokens control the style of description (general vs OCR).
func (p *Pipeline) describeImage(ctx context.Context, visionModel *config.ModelConfig,
	imageURL, prompt string, maxTokens int) (string, error) {

	// Use a dedicated timeout instead of the caller's context. The caller's
	// context is tied to the client connection, which may be closed (e.g. Claude
	// Code retry) before the vision model finishes. 120s gives large images and
	// busy vision backends enough headroom while still bounding the call. This
	// pairs with the dedicated vision client's 180s response-header timeout
	// (see visionResponseHeaderTimeout in pipeline.go): the context is the
	// outer bound, the transport timeout the inner one.
	visionCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_ = ctx // original context intentionally unused

	start := time.Now()

	// Fetch http(s) images in-process via the SSRF-safe client and forward
	// the bytes inline as a data: URL. This eliminates DNS rebinding (the
	// upstream never resolves the remote hostname) and removes our
	// dependence on the vision model's own SSRF protection, if any.
	forwardedURL, err := fetchImageAsDataURL(visionCtx, imageURL)
	if err != nil {
		return "", fmt.Errorf("fetch image: %w", err)
	}

	reqBody := map[string]any{
		"model": visionModel.Model,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": prompt,
					},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": forwardedURL,
						},
					},
				},
			},
		},
		"max_completion_tokens": maxTokens,
		// Disable reasoning/thinking for vision utility calls — we want all
		// tokens spent on the description, not internal chain-of-thought.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal vision request: %w", err)
	}

	url := visionModel.Backend + api.ChatCompletionsPath
	req, err := http.NewRequestWithContext(visionCtx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build vision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if visionModel.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+visionModel.APIKey)
	}

	// Use the dedicated vision client (longer response-header timeout) when
	// available, falling back to the general client for callers that construct
	// the Pipeline without one (e.g. tests using http.DefaultClient).
	client := p.visionClient
	if client == nil {
		client = p.client
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision model request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
	if err != nil {
		return "", fmt.Errorf("read vision response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("vision model error", "status", resp.StatusCode, "body", string(respBody))
		return "", fmt.Errorf("vision model returned HTTP %d", resp.StatusCode)
	}

	var chatResp struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("parse vision response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("vision model returned empty response")
	}

	// Prefer the final content. Fall back to reasoning_content when the
	// backend is a reasoning model (e.g., Qwen3-VL variants) that put the
	// actual description into its thinking channel — commonly happens when
	// finish_reason=length truncates the response mid-reasoning before the
	// model emits a final answer. The reasoning text is still useful
	// extraction output from the model's perspective; surfacing it prevents
	// the cascade from failing unnecessarily.
	msg := chatResp.Choices[0].Message
	desc := msg.Content
	source := "content"
	if strings.TrimSpace(desc) == "" && strings.TrimSpace(msg.ReasoningContent) != "" {
		desc = msg.ReasoningContent
		source = "reasoning_content"
		slog.Debug("vision model emitted only reasoning_content; using it as description",
			"vision_model", visionModel.Name,
			"finish_reason", chatResp.Choices[0].FinishReason)
	}
	// Detect truncation: if the vision model hit its token cap (finish_reason=length)
	// while emitting the content channel, the answer is a partial fragment, not a
	// complete description. Treat it as a failure so the cascade can retry (and so
	// the truncated fragment is never cached). When maxTokens is generous, an
	// intentional long response can still be cut short here and retried — acceptable,
	// since a retry is cheap and the fragment is never surfaced as-is.
	//
	// Only applies when content was the source. When we fell back to
	// reasoning_content, the model (e.g. Qwen3-VL) put its description in the
	// thinking channel — a known pattern where finish_reason=length is common and
	// the reasoning text is a complete, useful description that should be surfaced.
	if chatResp.Choices[0].FinishReason == "length" && source == "content" {
		return "", fmt.Errorf("vision model response truncated (finish_reason=length)")
	}
	if strings.TrimSpace(desc) == "" {
		return "", fmt.Errorf("vision model returned empty response")
	}

	slog.Debug("image described by vision model",
		"vision_model", visionModel.Name,
		"duration", time.Since(start),
		"description_len", len(desc),
		"source", source)

	return desc, nil
}

// RequestContainsImageURLs checks if a translated Chat Completions request
// contains any image_url content parts. Handles both []any and []map[string]any
// message slice types (depending on which handler built the request).
func RequestContainsImageURLs(chatReq map[string]any) bool {
	// Try []any first (used by pipeline and responses handler).
	if msgs, ok := chatReq["messages"].([]any); ok {
		for _, msg := range msgs {
			if hasImageURLParts(msg) {
				return true
			}
		}
	}
	// Try []map[string]any (used by messages_translate).
	if msgs, ok := chatReq["messages"].([]map[string]any); ok {
		for _, msg := range msgs {
			if hasImageURLParts(msg) {
				return true
			}
		}
	}
	return false
}

// hasImageURLParts checks if a single message (as any) contains image_url content parts.
func hasImageURLParts(msg any) bool {
	m, ok := msg.(map[string]any)
	if !ok {
		return false
	}
	parts := normalizeContentParts(m)
	if parts == nil {
		return false
	}
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if ok && p["type"] == "image_url" {
			return true
		}
	}
	return false
}
