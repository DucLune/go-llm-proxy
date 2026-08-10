package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go-llm-proxy/internal/config"
)

// InternalKeyStrippedTools is the map key used to pass stripped server tool types
// from the translation layer to the pipeline. Deleted before sending to backend.
const InternalKeyStrippedTools = "_stripped_server_tools"

// visionResponseHeaderTimeout bounds how long the pipeline waits for the vision
// model to return response headers. This is deliberately much longer than the
// general proxy client's 30s (see internal/httputil): vision models routinely
// take 6-10s per image and can be slower under concurrent load, and a too-short
// timeout here silently downgrades images to "[Image could not be processed]".
const visionResponseHeaderTimeout = 180 * time.Second

// Pipeline orchestrates pre-send content processing for translated Chat Completions requests.
// It detects unsupported content (images, PDFs) and routes them to capable processor models.
type Pipeline struct {
	config *config.ConfigStore
	client *http.Client
	// visionClient is a dedicated HTTP client for vision/OCR model calls. It uses
	// a longer response-header timeout than the general client so slow vision
	// models (large images, concurrent batches) aren't cut off mid-description.
	// Falls back to client when nil (tests, callers that build the Pipeline
	// without a vision client).
	visionClient *http.Client
	// mineruClient is the MinerU cloud client used for PDF extraction. Built
	// lazily from the processors config on first use (so config reloads take
	// effect); tests may pre-set it to point at a mock server.
	mineruClient *MinerUClient
}

// NewPipeline creates a pipeline that uses the given config and HTTP client for processor calls.
// A dedicated vision client with a longer response-header timeout is created for
// vision-model calls so that slow vision backends aren't bounded by the general
// client's 30s timeout (which previously caused images to be downgraded to
// "[Image could not be processed]").
func NewPipeline(cs *config.ConfigStore, client *http.Client) *Pipeline {
	return &Pipeline{config: cs, client: client, visionClient: newVisionClient()}
}

// newVisionClient returns an HTTP client for vision-model calls with a long
// response-header timeout. Shares dial/TLS tuning with the general client but
// tolerates the slow, token-heavy responses vision models produce.
func newVisionClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: visionResponseHeaderTimeout,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// processingSignatures are byte patterns that indicate a request body may contain
// content that needs pipeline processing. Used for cheap pre-scan before full JSON parse.
var processingSignatures = [][]byte{
	[]byte(`"image_url"`),       // OpenAI image format
	[]byte(`"type":"image"`),    // Anthropic image format (after translation)
	[]byte(`"application/pdf"`), // PDF media type
	[]byte(`JVBERi0`),           // PDF magic bytes in base64
	[]byte(`"type":"document"`), // Anthropic document format
	[]byte(`"pdf_data"`),        // Pipeline-internal PDF marker (after translation)
}

// BodyNeedsProcessing does a fast string scan to detect if the raw request body
// contains content that may need pipeline processing. This avoids full JSON parse
// for the common case of text-only requests.
func (p *Pipeline) BodyNeedsProcessing(body []byte) bool {
	for _, sig := range processingSignatures {
		if bytes.Contains(body, sig) {
			return true
		}
	}
	return false
}

// ShouldProcess returns whether the pipeline should run for the given model.
// Native Anthropic backends skip the pipeline unless force_pipeline is set.
func (p *Pipeline) ShouldProcess(model *config.ModelConfig) bool {
	if model.Type == config.BackendAnthropic && !model.ForcePipeline {
		return false
	}
	return true
}

// resolveVisionProcessor returns the model name to use for vision processing
// for the given target model. Returns "" if vision processing is disabled.
func (p *Pipeline) resolveVisionProcessor(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil {
		if targetModel.Processors.Vision == "none" {
			return ""
		}
		if targetModel.Processors.Vision != "" {
			return targetModel.Processors.Vision
		}
	}
	// Fall back to global config.
	return p.config.Get().Processors.Vision
}

// resolveOCRProcessor returns the model name to use for OCR processing
// (PDF page images). Falls back to the vision processor if no OCR model is set.
// Returns "" if both are disabled.
func (p *Pipeline) resolveOCRProcessor(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil {
		if targetModel.Processors.OCR == "none" {
			return ""
		}
		if targetModel.Processors.OCR != "" {
			return targetModel.Processors.OCR
		}
	}
	// Fall back to global OCR config.
	if ocr := p.config.Get().Processors.OCR; ocr != "" {
		return ocr
	}
	// Fall back to vision processor.
	return p.resolveVisionProcessor(targetModel)
}

// ResolveWebSearchKey returns the Tavily API key for the given target model.
// Returns "" if web search is disabled for this model.
func (p *Pipeline) ResolveWebSearchKey(targetModel *config.ModelConfig) string {
	// Per-model override takes precedence.
	if targetModel.Processors != nil && targetModel.Processors.WebSearchKey != "" {
		if targetModel.Processors.WebSearchKey == "none" {
			return ""
		}
		return targetModel.Processors.WebSearchKey
	}
	// Fall back to global config.
	return p.config.Get().Processors.WebSearchKey
}

// ProcessRequest runs pre-send processors on a translated Chat Completions request.
// It modifies the request in place and returns it.
func (p *Pipeline) ProcessRequest(ctx context.Context, chatReq map[string]any,
	targetModel *config.ModelConfig) (map[string]any, error) {

	if !p.ShouldProcess(targetModel) {
		return chatReq, nil
	}

	cfg := p.config.Get()

	// Resolve the vision model once (used by both image and PDF processing).
	visionModelName := p.resolveVisionProcessor(targetModel)
	var visionModel *config.ModelConfig
	if visionModelName != "" {
		visionModel = config.FindModel(cfg, visionModelName)
	}

	// Resolve the OCR model (used for PDF page images). Falls back to vision model.
	ocrModelName := p.resolveOCRProcessor(targetModel)
	var ocrModel *config.ModelConfig
	if ocrModelName != "" {
		ocrModel = config.FindModel(cfg, ocrModelName)
	}

	// Normalize PDF data URLs disguised as image_url into pdf_data parts.
	// Runs before both image and PDF processors so that Chat Completions and
	// Responses API clients (which have no structured PDF input) converge on
	// the same internal shape as Anthropic's document blocks.
	NormalizePDFDataURLs(chatReq)

	// Vision: route images to processor if target can't handle them natively.
	// Skip if the vision model IS the target (avoid pointless round-trip).
	if visionModel != nil && visionModel.Name != targetModel.Name && (!targetModel.SupportsVision || targetModel.ForcePipeline) {
		var err error
		// processors.vision_max_tokens (0 = built-in defaults) caps how many
		// tokens the vision model may spend describing each image.
		visionMaxTokens := cfg.Processors.VisionMaxTokens
		// processors.max_images_per_request (0 = built-in default of 10) caps
		// how many unique images a single request may process.
		maxImagesPerRequest := cfg.Processors.MaxImagesPerRequest
		chatReq, err = p.processImages(ctx, chatReq, visionModel, ocrModel, visionMaxTokens, maxImagesPerRequest)
		if err != nil {
			slog.Warn("vision processing error", "error", err)
		}
	}

	// PDF: text extraction via MinerU (always attempted when configured), with
	// embedded images described by the vision model. ocrModel is preserved in
	// the signature for processImages; MinerU extraction does not use it.
	{
		var err error
		visionMaxTokens := cfg.Processors.VisionMaxTokens
		chatReq, err = p.processPDFs(ctx, chatReq, visionModel, ocrModel, visionMaxTokens)
		if err != nil {
			slog.Warn("PDF processing error", "error", err)
		}
	}

	// Web search: convert stripped server tools to function tools, or inject.
	chatReq = p.convertOrInjectSearchTool(chatReq, targetModel)

	// Clean up internal metadata that shouldn't be sent to the backend.
	delete(chatReq, InternalKeyStrippedTools)

	return chatReq, nil
}

// pipelineError builds a formatted error message for pipeline failures.
func pipelineError(feature, model, docSection, originalErr string) string {
	return fmt.Sprintf(
		"The backend model (%s) does not support %s, and the proxy could not process it.\n\n"+
			"To enable %s support, configure the proxy:\n\n"+
			"    processors:\n"+
			"      %s\n\n"+
			"Original error:\n    %s",
		model, feature, feature, docSection, originalErr,
	)
}

// imageNotSupportedError returns a friendly error when images are sent to a text-only
// model and no vision processor is configured.
func imageNotSupportedError(modelName string, originalErr string) string {
	return pipelineError("image inputs", modelName,
		"vision: your-vision-model    # any vision-capable model", originalErr)
}

// searchNotConfiguredError returns a friendly error when web search is requested
// but no Tavily key is configured.
func searchNotConfiguredError() string {
	return "Web search was requested but no search API key is configured on the proxy.\n\n" +
		"To enable web search, add a Tavily API key to your proxy config:\n\n" +
		"    processors:\n" +
		"      web_search_key: tvly-your-key"
}
