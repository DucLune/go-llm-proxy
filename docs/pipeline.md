# Processing Pipeline

go-llm-proxy includes a content processing pipeline that transparently handles images, PDFs, and web search for backends that don't support them natively. The pipeline runs automatically when configured — no client-side changes needed.

## Configuration

```yaml
processors:
  vision: Qwen3-VL-8B           # vision-capable model for image description
  ocr: paddleOCR                # dedicated OCR model for tool-output page images (optional)
  web_search_key: tvly-...      # Tavily API key for web search (optional)
  mineru_api_key: your-key      # MinerU cloud API token — enables PDF extraction
```

The `vision` model is required for image processing. The `ocr` model is optional — if not configured, the vision model handles OCR duties as a fallback. PDF extraction requires `mineru_api_key` (see [PDF processing](#pdf-processing) below). See [config-reference.md](config-reference.md) for per-model overrides and additional options.

No system dependencies are required for PDF processing — extraction runs through the MinerU cloud API, so the host needs neither poppler-utils nor ghostscript. (Rasterizer tools are no longer used by the proxy since the local OCR-rasterization cascade was replaced.)

## Image processing

When a client sends an image to a text-only backend, the proxy intercepts the image, sends it to a vision-capable model for description, and replaces the image with the text description before forwarding to the backend.

### How images are handled per role

| Image source | Processing | Pipeline | Prompt |
|---|---|---|---|
| **User-attached image** (user message) | Vision description | `vision` model only — no cascade | Describe the image accurately and objectively |
| **Tool output image** (tool message — Codex `view_image`, screenshots) | OCR → vision cascade | `ocr` model first, then `vision` on failure | `OCR:` (dedicated OCR) then verbose extraction prompt on fallback |

User-attached photos receive only vision description — dedicated OCR models produce unreliable output on natural photographs. Text visible in photos is captured adequately by the vision model's description.

Tool output images (Codex `view_image` results, screenshots) first go to the dedicated OCR model. If OCR returns empty, errors, or the `ocr` model is unavailable, the proxy automatically retries via the `vision` model. When the operator has configured only one processor, the pipeline detects the overlap and avoids duplicate calls to the same backend.

### Output format

Injected content is wrapped in XML-like tags so target models clearly distinguish pipeline-sourced text from user-authored content:

- `<image_description>...</image_description>` — user-role vision description
- `<page_text>...</page_text>` — tool-role OCR/vision extraction
- `<pdf_content filename="..." source="mineru">...</pdf_content>` — PDF extraction (see below)

### Processing details

- Images are processed **concurrently** (up to 5 in parallel)
- Successful results are **cached by content hash** — follow-up turns with the same image are instant. The cache evicts **least-recently-used entries** (max 1024) rather than clearing wholesale, so historical images stay warm across turns even as a conversation grows
- Vision-model calls use a **dedicated HTTP client** with a 180s response-header timeout (vs. 30s for the general client), so slow vision backends aren't cut off mid-description
- Failed extractions are cached for **5 minutes** so transient upstream issues don't permanently block an image but a misbehaving client can't hammer the cascade every turn
- Maximum unique images per request defaults to **10** (configurable via `processors.max_images_per_request`); additional images get a placeholder
- Cache keys include a mode suffix (`:v` for vision, `:o` for OCR, `:fail` for the short-TTL failure marker) so results are stored independently
- Reasoning/thinking is disabled for vision model calls to maximize output quality

## PDF processing

PDF content is extracted through the [MinerU cloud API](https://mineru.net/apiManage/docs) (structure-preserving document parsing) plus the vision model for embedded images. This is the **only** PDF path — the old local text/OCR/vision cascade was removed.

Requires `processors.mineru_api_key`. Without it, every PDF block is replaced with `[PDF: MinerU processing failed — mineru_api_key not configured]` and an error is logged. If the MinerU API is unreachable, the proxy substitutes a placeholder and opens a circuit breaker so repeated requests fail fast instead of hammering the cloud API.

### How PDF extraction works

For each PDF block the proxy:

1. **Submits** the PDF to MinerU (`POST /api/v4/file-urls/batch` → `PUT` the signed upload URL → poll `GET /api/v4/extract-results/batch/{id}` until `done` → download the result zip)
2. **Describes embedded images**: MinerU crops figures and tables to `images/*.jpg`; the proxy sends each cropped image to the `vision` model (see [Image processing](#image-processing)) and replaces the markdown `![](images/...)` reference with the description, prefixed by MinerU's caption when present (`[图: caption]\n<description>`). The image descriptions run **concurrently** (up to 5 in parallel, the same cap as user images) so figure-heavy PDFs aren't serialized into minutes of vision calls — a 19-figure PDF drops from ~10-15 min serial to ~3 min concurrent
3. **Wraps the result** in `<pdf_content filename="..." source="mineru">...</pdf_content>` and caches it by content hash

The `source="mineru"` attribute identifies the extraction path so downstream logs and debugging sessions can see it without replaying the call.

### Failure behavior

MinerU failures are **not** silently swallowed:

- The PDF block is replaced with `[PDF: MinerU processing failed — <reason>]` — no fallback to the old extraction cascade
- An `ERROR`-level log line (`mineru PDF processing failed`) is emitted with filename, error, and byte count
- A circuit breaker opens after 10 consecutive failures (30s cooldown), after which PDF requests short-circuit to the placeholder instead of waiting for the full cloud timeout
- Failed results are cached for 5 minutes so a broken upstream doesn't permanently block a document but a misconfigured client can't retrigger it every turn
- The result-zip **download is retried** on transient network errors (connection/TLS failure, HTTP 5xx) — `processors.mineru_download_retries`, default 1 (2 attempts total). HTTP 4xx (e.g. an expired signed URL) is not retried; the URL won't become valid on a second attempt

### Client entry points

| Client | How PDFs reach the pipeline |
|---|---|
| **Claude Code** (Anthropic) | Sends base64 `document` blocks → translated to `pdf_data` → MinerU extraction |
| **Chat Completions** | PDFs submitted as `image_url` with a `data:application/pdf;base64,...` URL are normalized to `pdf_data` → same MinerU path |
| **Codex CLI** | Typically handles PDFs client-side (`pdftotext`, then `pdfimages` + `view_image` per page, processed through the tool-role image cascade). If a PDF is submitted directly as `input_image` with a `data:application/pdf` URL, the Responses translator converts it to `pdf_data` → MinerU path |
| **OpenCode / Qwen Code** | Handle PDFs entirely client-side; the proxy's MinerU path runs only for direct requests containing PDF signatures |

### Supported PDF types

MinerU extracts structure from all of them — native-text, scanned/image, and mixed PDFs. Text pages yield clean markdown; scanned pages go through MinerU's OCR; figures/tables are cropped and described by the vision model. Per-page quality depends on the `mineru_model_version` (`pipeline` = fast, `vlm` = slower but more accurate).

## Web search

When a client includes a web search tool, the proxy intercepts it, executes the search, and injects results into the conversation. The backend model sees only the final response with search context incorporated.

### Supported providers

The proxy auto-detects the search provider from the `web_search_key` prefix:

| Provider | Key prefix | Free tier | Notes |
|---|---|---|---|
| [Tavily](https://tavily.com/) | `tvly-` | 1,000 req/month | Includes AI-generated answer summary |
| [Brave Search](https://brave.com/search/api/) | `BSA` | $5/month credit (~1,000 req) | Independent index, privacy-focused |

### Per-client behavior

| Client | Proxy-side (Tavily or Brave) | Client-side fallback |
|---|---|---|
| **Claude Code** | Automatic — proxy intercepts `web_search_20250305` server tool | Tavily only (via MCP) |
| **Codex CLI** | Automatic — proxy intercepts `web_search` server tool | Tavily only (via MCP) |
| **OpenCode** | Automatic — proxy serves `/mcp/sse` endpoint | Tavily only (via MCP) |
| **Qwen Code** | Via MCP — proxy serves `/mcp/sse` endpoint | Tavily, Google, or DashScope |

**Note:** Brave Search is only available through the proxy (`web_search_key` in config.yaml). Client-side search configs only support Tavily because there is no Brave MCP endpoint. If you want Brave Search, configure it on the proxy and all clients will use it automatically via their respective mechanisms.

## Recommended models

| Role | Recommended | Parameters | Notes |
|---|---|---|---|
| **Vision** | [Qwen3-VL-8B](https://huggingface.co/Qwen/Qwen3-VL-8B-Instruct) | 8B | Best quality/speed balance for image description (user images, MinerU cropped figures) |
| **OCR** | [PaddleOCR-VL-1.5](https://huggingface.co/PaddlePaddle/PaddleOCR-VL-1.5) | 0.9B | Purpose-built for text extraction from tool-output page images (`view_image`), 94.5% accuracy, ~2s/page. Not used for PDFs (those go through MinerU) |
| **OCR (alt)** | [DeepSeek-OCR 2](https://huggingface.co/deepseek-ai/DeepSeek-OCR) | 3B | Higher accuracy (97%), layout analysis, table extraction |

## Caching

All pipeline results are cached by content hash for the lifetime of the proxy process:

- **Image descriptions**: cached per image URL hash + mode (`:v` or `:o`)
- **PDF extraction** (MinerU markdown + image descriptions): cached per PDF content hash
- **PDF failures** (placeholder): cached per PDF content hash with a 5-minute TTL

Cache is in-memory only and resets on proxy restart. It is bounded to **1024 entries** per cache; when full, the least-recently-used entry is evicted to make room (no wholesale flush). This keeps memory bounded while preserving recent descriptions — historical images stay warm across turns instead of forcing the vision model to re-describe them every time Claude Code replays conversation history.

## Pipeline flow

```
Client request
  → Protocol handler parses request
  → Translate to Chat Completions (if needed)
  → Pipeline: describe images (vision model)
  → Pipeline: OCR tool-output images (OCR model)
  → Pipeline: extract PDFs via MinerU + describe embedded images
  → Pipeline: inject web search function tool
  → Send to backend
  → Pipeline: execute web search if called, re-send with results
  → Translate response back to client protocol
  → Stream to client
```
