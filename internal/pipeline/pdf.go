package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go-llm-proxy/internal/config"
)

// pdfFailureTTL is how long a failure result is cached. Short enough
// that transient upstream issues don't permanently poison the cache, long
// enough that a spammy retry loop from a misbehaving client won't hammer
// the MinerU cloud API on every turn.
const pdfFailureTTL = 5 * time.Minute

// pdfCache stores PDF data hash → extracted/described text so that PDFs are only
// processed once. Subsequent requests containing the same PDF reuse the cached
// result, avoiding repeated extraction and vision model calls. Bounded to
// prevent unbounded memory growth in long-running processes.
var pdfCache = newBoundedCache()

// ResetPDFCache clears the PDF description cache. Exported for testing.
func ResetPDFCache() {
	pdfCache.Reset()
}

// maxPDFTextLength caps extracted text to avoid overwhelming the model's context.
const maxPDFTextLength = 100_000

// processPDFs detects PDF content in the translated Chat Completions request,
// extracts structure/text via the MinerU cloud API, describes any embedded
// images with the vision model, and replaces the PDF with a pure-text block.
//
// ocrModel is intentionally unused here — the old rasterize+OCR cascade was
// replaced by MinerU. The parameter stays so the signature matches the caller;
// processImages still needs ocrModel for tool-role images.
func (p *Pipeline) processPDFs(ctx context.Context, chatReq map[string]any,
	visionModel *config.ModelConfig, ocrModel *config.ModelConfig, visionMaxTokens int) (map[string]any, error) {

	_ = ocrModel

	// Normalize messages to []any (same pattern as processImages).
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

	anyModified := false
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}

		msgModified := false
		newContent := make([]any, 0, len(content))
		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok {
				newContent = append(newContent, part)
				continue
			}
			if partMap["type"] != "pdf_data" {
				newContent = append(newContent, part)
				continue
			}

			slog.Info("processing PDF content block")

			// Extract PDF data.
			b64Data, _ := partMap["data"].(string)
			filename, _ := partMap["filename"].(string)
			if b64Data == "" {
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[PDF: no data provided]",
				})
				msgModified = true
				continue
			}

			// Check the cache first — avoid re-processing the same PDF
			// on every conversational turn.
			pdfCacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(b64Data)))
			if cached, ok := pdfCache.Load(pdfCacheKey); ok {
				slog.Debug("PDF cache hit", "filename", filename)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": cached,
				})
				msgModified = true
				continue
			}

			// Try standard base64 first, then URL-safe, then with whitespace stripped.
			pdfBytes, err := decodePDFBase64(b64Data)
			if err != nil {
				slog.Warn("failed to decode PDF base64", "error", err, "data_len", len(b64Data))
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": "[PDF content could not be decoded]",
				})
				msgModified = true
				continue
			}
			slog.Info("PDF decoded", "filename", filename, "pdf_bytes", len(pdfBytes))

			// MinerU extraction. This is the only PDF path now — there is no
			// fallback to the old text/OCR/vision cascade.
			mineru := p.mineruClient
			if mineru == nil {
				mineru = NewMinerUClient(p.config.Get().Processors, p.client)
			}
			res, err := mineru.ProcessPDFBytes(ctx, pdfBytes)
			if err != nil {
				slog.Error("mineru PDF processing failed",
					"filename", filename, "error", err, "pdf_bytes", len(pdfBytes))
				failResult := mineruFailurePlaceholder(err)
				pdfCache.StoreWithTTL(pdfCacheKey, failResult, pdfFailureTTL)
				newContent = append(newContent, map[string]any{
					"type": "text",
					"text": failResult,
				})
				msgModified = true
				continue
			}

			// Turn embedded images into text descriptions (pure-text output).
			text := p.describeMineruImages(ctx, res, visionModel, visionMaxTokens)
			if len(text) > maxPDFTextLength {
				text = text[:maxPDFTextLength] + "\n\n[PDF text truncated at 100K characters]"
			}
			result := buildPDFResult(filename, "mineru", text)
			pdfCache.Store(pdfCacheKey, result)
			newContent = append(newContent, map[string]any{
				"type": "text",
				"text": result,
			})
			slog.Info("PDF processed via MinerU",
				"filename", filename, "text_len", len(text))
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

// mineruFailurePlaceholder is the text substituted for a PDF when MinerU
// extraction fails. The reason is included so downstream models and operators
// can distinguish a real failure from a successful extraction.
func mineruFailurePlaceholder(err error) string {
	reason := "unknown error"
	if err != nil {
		reason = err.Error()
	}
	return fmt.Sprintf("[PDF: MinerU processing failed — %s]", reason)
}

// mineruImageRef matches the Markdown image references MinerU emits:
//
//	![](images/<hashed-name>.jpg)
var mineruImageRef = regexp.MustCompile(`!\[[^\]]*\]\(images/[^)\s]+\)`)

// describeMineruImages replaces every image reference in the MinerU markdown
// with a vision-model description of the cropped image, so the output is pure
// text usable by text-only LLMs.
//
// The vision model and prompt are the same ones used for user images
// (processors.vision + describeImage). A caption from MinerU's layout analysis
// (when present) prefixes the description. A missing vision model or a
// per-image description failure degrades to caption-only/empty rather than
// failing the whole PDF — MinerU's text extraction is the primary win.
//
// The description calls run concurrently (bounded by maxConcurrentVision, the
// same cap processImages uses) so a figure-heavy PDF isn't serialized into
// minutes of vision calls. Reusing maxConcurrentVision is safe: ProcessRequest
// runs processImages before processPDFs and the two never overlap, so MinerU
// descriptions never double the concurrent load on the vision backend. If that
// invariant ever changes, give MinerU its own cap.
func (p *Pipeline) describeMineruImages(ctx context.Context, res *mineruResult, visionModel *config.ModelConfig, visionMaxTokens int) string {
	md := res.markdown
	if visionModel == nil {
		slog.Warn("no vision model configured; MinerU images left undescribed",
			"image_refs", len(mineruImageRef.FindAllString(md, -1)))
		return mineruImageRef.ReplaceAllString(md, "")
	}

	captions := mineruImageCaptions(res.files)

	// Collect unique image paths in markdown order. A reference whose file is
	// missing from the zip is recorded immediately (deterministic log) so only
	// existing images reach the worker pool.
	seen := map[string]bool{}
	descriptions := map[string]string{}
	var paths []string
	for _, ref := range mineruImageRef.FindAllString(md, -1) {
		path := mineruImagePath(ref)
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, ok := res.files[path]; !ok {
			slog.Warn("MinerU image missing from result zip", "path", path)
			descriptions[path] = ""
			continue
		}
		paths = append(paths, path)
	}

	// Describe each image concurrently, bounded by the shared vision cap.
	// Each worker writes only its own slot; done[] marks completion so the
	// collector reads no slot still being written (avoids the race present in
	// processImages' escape path).
	type imageResult struct {
		desc string
		err  error
	}
	if len(paths) > 0 {
		results := make([]imageResult, len(paths))
		done := make([]atomic.Bool, len(paths))
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxConcurrentVision)

		for i, path := range paths {
			wg.Add(1)
			go func(idx int, path string) {
				defer wg.Done()
				select {
				case <-ctx.Done():
					// Parent context cancelled (client disconnect) while
					// waiting for a slot — record and exit without calling the
					// vision backend.
					results[idx] = imageResult{err: ctx.Err()}
					done[idx].Store(true)
					return
				case sem <- struct{}{}:
				}
				defer func() { <-sem }()

				// Circuit breaker: if the vision backend is in a failure streak,
				// short-circuit instead of spinning up calls that will time out
				// (aligns with processImages' worker behavior).
				if !visionBreaker.Allow() {
					results[idx] = imageResult{err: fmt.Errorf("vision backend temporarily unavailable (circuit breaker open)")}
					done[idx].Store(true)
					return
				}

				desc, err := p.describeImage(ctx, visionModel, mineruImageDataURL(path, res.files[path]),
					visionPromptDescribe, maxTokensForRole("tool", visionMaxTokens))
				results[idx] = imageResult{desc: desc, err: err}
				done[idx].Store(true)
			}(i, path)
		}

		// Wait for all workers, but don't block forever if the parent context is
		// cancelled. describeImage ignores the parent ctx (it has its own 120s
		// timeout), so an in-flight worker may not return promptly — escape on
		// ctx.Done and leave stragglers to finish in the background (bounded by
		// their own 120s timeout). Collect only completed slots.
		allDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(allDone)
		}()
		select {
		case <-allDone:
		case <-ctx.Done():
			slog.Warn("MinerU image description cancelled by context", "error", ctx.Err())
		}

		// Collect results single-threaded so breaker Success/Failure and the
		// failed count are deterministic.
		failed := 0
		for i, path := range paths {
			if !done[i].Load() {
				continue // still in flight after ctx escape — leave caption-only
			}
			r := results[i]
			if r.err != nil || strings.TrimSpace(r.desc) == "" {
				slog.Warn("failed to describe MinerU image",
					"path", path, "vision_model", visionModel.Name, "error", r.err)
				visionBreaker.Failure()
				failed++
				descriptions[path] = ""
				continue
			}
			visionBreaker.Success()
			slog.Debug("described MinerU image",
				"path", path, "vision_model", visionModel.Name, "desc_len", len(r.desc))
			descriptions[path] = r.desc
		}
		if failed > 0 {
			slog.Warn("some MinerU images could not be described",
				"failed", failed, "vision_model", visionModel.Name)
		}
	}

	// Substitute each reference in order. Completion order is irrelevant here —
	// descriptions is fully populated by this point and ReplaceAllStringFunc
	// walks the markdown left to right.
	return mineruImageRef.ReplaceAllStringFunc(md, func(ref string) string {
		desc := descriptions[mineruImagePath(ref)]
		caption := captions[mineruImagePath(ref)]
		switch {
		case desc != "" && caption != "":
			return fmt.Sprintf("\n[图: %s]\n%s\n", caption, desc)
		case desc != "":
			return fmt.Sprintf("\n[图]\n%s\n", desc)
		case caption != "":
			return fmt.Sprintf("\n[图: %s]\n", caption)
		default:
			return ""
		}
	})
}

// mineruImagePath extracts the "images/<file>" path from a MinerU image
// reference like "![](images/abc.jpg)".
func mineruImagePath(ref string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ref, "![]("), ")")
}

// mineruImageCaptions parses the result zip's *_content_list_v2.json and maps
// each cropped image path to its caption (or footnote) as recorded by MinerU's
// layout analysis.
func mineruImageCaptions(files map[string][]byte) map[string]string {
	captions := map[string]string{}
	for name, data := range files {
		if !strings.HasSuffix(name, "_content_list_v2.json") {
			continue
		}
		// v2 layout: top-level is a list of pages; each page is a list of blocks.
		var pages []any
		if err := json.Unmarshal(data, &pages); err != nil {
			slog.Warn("failed to parse MinerU content_list_v2", "file", name, "error", err)
			continue
		}
		for _, pg := range pages {
			blocks, ok := pg.([]any)
			if !ok {
				continue
			}
			for _, blk := range blocks {
				b, ok := blk.(map[string]any)
				if !ok {
					continue
				}
				btype, _ := b["type"].(string)
				if btype != "image" && btype != "chart" {
					continue
				}
				content, _ := b["content"].(map[string]any)
				if content == nil {
					continue
				}
				src, _ := content["image_source"].(map[string]any)
				path, _ := src["path"].(string)
				if path == "" {
					continue
				}
				caption := joinMineruText(content["image_caption"])
				if caption == "" {
					caption = joinMineruText(content["image_footnote"])
				}
				captions[path] = caption
			}
		}
		break // only the first content_list_v2
	}
	return captions
}

// joinMineruText concatenates the text content of a MinerU content block
// (a list of {type, content} items) into a single string.
func joinMineruText(v any) string {
	items, ok := v.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := m["content"].(string); ok && strings.TrimSpace(t) != "" {
			parts = append(parts, strings.TrimSpace(t))
		}
	}
	return strings.Join(parts, " ")
}

// mineruImageDataURL wraps cropped-image bytes as a data URL so describeImage
// can send them inline (safe_http.go returns data: URLs unchanged).
func mineruImageDataURL(path string, data []byte) string {
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".svg":
		mime = "image/svg+xml"
	case ".bmp":
		mime = "image/bmp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// DecodePDFDataURL detects URLs of the form `data:application/pdf;base64,...`
// and returns the base64 payload (without the prefix). Returns false if the
// URL is not a PDF data URL or is malformed. Exported for use by API-layer
// translators that need to recognize PDFs masquerading as images.
func DecodePDFDataURL(url string) (string, bool) {
	const prefix = "data:application/pdf"
	if !strings.HasPrefix(url, prefix) {
		return "", false
	}
	// Find the comma that separates the header from the payload.
	idx := strings.IndexByte(url, ',')
	if idx < 0 {
		return "", false
	}
	// Expect base64 encoding in the header — it's the only encoding the
	// pipeline supports. If a client sends a URL-encoded or plaintext PDF
	// data URL, skip (the shape is never valid for PDFs anyway).
	header := url[:idx]
	if !strings.Contains(header, ";base64") {
		return "", false
	}
	payload := url[idx+1:]
	if payload == "" {
		return "", false
	}
	return payload, true
}

// NormalizePDFDataURLs walks a Chat Completions message list and rewrites
// any image_url parts whose URL is a PDF data URL into pipeline-internal
// pdf_data parts. Idempotent and inexpensive when no PDFs are present
// (early-exits on the first non-matching part). Intended to be called just
// before processPDFs so all three entry APIs (Anthropic Messages,
// Chat Completions, OpenAI Responses) converge on the same internal shape
// regardless of how the client originally submitted the PDF.
func NormalizePDFDataURLs(chatReq map[string]any) {
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
		return
	}

	anyModified := false
	for i, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content := normalizeContentParts(msgMap)
		if content == nil {
			continue
		}
		msgModified := false
		newContent := make([]any, 0, len(content))
		for _, part := range content {
			partMap, ok := part.(map[string]any)
			if !ok || partMap["type"] != "image_url" {
				newContent = append(newContent, part)
				continue
			}
			url := extractImageURL(partMap)
			data, isPDF := DecodePDFDataURL(url)
			if !isPDF {
				newContent = append(newContent, part)
				continue
			}
			// Replace image_url with pdf_data. Preserve filename hint if
			// the client supplied one via the (non-standard but seen-in-wild)
			// "filename" sibling field.
			converted := map[string]any{
				"type": "pdf_data",
				"data": data,
			}
			if fn, ok := partMap["filename"].(string); ok && fn != "" {
				converted["filename"] = fn
			}
			newContent = append(newContent, converted)
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
}

// decodePDFBase64 decodes base64-encoded PDF data, trying multiple encodings.
func decodePDFBase64(data string) ([]byte, error) {
	// Standard base64.
	if b, err := base64.StdEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// URL-safe base64.
	if b, err := base64.URLEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// Raw (no padding) variants.
	if b, err := base64.RawStdEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	// Strip whitespace and retry.
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, data)
	if b, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(cleaned)
}

// buildPDFResult formats extracted/described PDF text with an XML-like
// wrapper so the target model can distinguish pipeline-injected content
// from user-authored text. The source attribute identifies which stage
// produced the text: "mineru" (MinerU cloud extraction).
func buildPDFResult(filename, source, content string) string {
	if filename != "" {
		return fmt.Sprintf("<pdf_content filename=%q source=%q>\n%s\n</pdf_content>",
			filename, source, content)
	}
	return fmt.Sprintf("<pdf_content source=%q>\n%s\n</pdf_content>", source, content)
}
