# Claude Code

Connect [Claude Code](https://claude.ai/download) to go-llm-proxy to use your self-hosted or third-party models as the backend. The proxy automatically translates between the Anthropic Messages API (which Claude Code speaks) and Chat Completions (which most local models speak).

## Quick start

The easiest path is the built-in config generator (`--serve-config-generator`). Select **Claude Code** from the dropdown, choose your models for Sonnet/Opus/Haiku slots, and generate a `settings.json` or start script.

## How it works

Claude Code uses the Anthropic Messages API exclusively. When you point it at the proxy:

- **Anthropic backends** (`type: anthropic`): requests pass through natively — full fidelity, including extended thinking with real signatures
- **OpenAI-compatible backends** (vLLM, llama-server, etc.): the proxy automatically translates Anthropic Messages → Chat Completions, and translates the response back. No configuration needed — it detects the backend type from your model config.

The translation handles:
- Text content and streaming (SSE event format translation)
- Tool calling round-trips (tool_use ↔ tool_calls, tool_result ↔ role:tool)
- Reasoning tokens → thinking blocks (models like MiniMax emit reasoning that appears as thinking in Claude Code)
- System prompts, stop sequences, temperature, max tokens
- Errors wrapped in Anthropic format

### `messages_mode`

Control the translation behavior per model:

| Value | Behavior |
|---|---|
| `auto` | Default. Anthropic backends passthrough, others translate automatically |
| `native` | Force passthrough (backend must speak Anthropic protocol) |
| `translate` | Force translation to Chat Completions |

Most users don't need to set this — `auto` handles everything correctly.

## Configuration file

Save as `~/.claude/settings.json`:

```json
{
  "attribution": { "commit": "", "pr": "" },
  "env": {
    "ANTHROPIC_BASE_URL": "https://your-proxy.example.com",
    "ANTHROPIC_API_KEY": "your-proxy-api-key",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "MiniMax-M2.5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "MiniMax M2.5",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES": "thinking,interleaved_thinking",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "qwen-3.5",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_NAME": "Qwen 3.5",
    "ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES": "thinking,interleaved_thinking",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "MiniMax-M2.5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME": "MiniMax M2.5",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES": "",
    "DISABLE_PROMPT_CACHING": "1",
    "CLAUDE_CODE_DISABLE_1M_CONTEXT": "1",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "API_TIMEOUT_MS": "900000"
  }
}
```

## Start script (alternative)

Instead of editing `settings.json`, use a start script that sets environment variables and launches Claude Code:

```bash
#!/usr/bin/env bash
exec env \
  ANTHROPIC_BASE_URL="https://your-proxy.example.com" \
  ANTHROPIC_API_KEY="your-proxy-api-key" \
  ANTHROPIC_DEFAULT_SONNET_MODEL="MiniMax-M2.5" \
  ANTHROPIC_DEFAULT_SONNET_MODEL_NAME="MiniMax M2.5" \
  ANTHROPIC_DEFAULT_SONNET_MODEL_SUPPORTED_CAPABILITIES="thinking,interleaved_thinking" \
  ANTHROPIC_DEFAULT_OPUS_MODEL="qwen-3.5" \
  ANTHROPIC_DEFAULT_OPUS_MODEL_NAME="Qwen 3.5" \
  ANTHROPIC_DEFAULT_OPUS_MODEL_SUPPORTED_CAPABILITIES="thinking,interleaved_thinking" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL="MiniMax-M2.5" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME="MiniMax M2.5" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL_SUPPORTED_CAPABILITIES="" \
  DISABLE_PROMPT_CACHING="1" \
  CLAUDE_CODE_DISABLE_1M_CONTEXT="1" \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1" \
  API_TIMEOUT_MS="900000" \
  claude --settings '{"attribution":{"commit":"","pr":""}}' "$@"
```

Save as `claude-proxy.sh`, make executable (`chmod +x`), and run.

## Key settings

| Variable | Purpose |
|---|---|
| `ANTHROPIC_BASE_URL` | Your proxy URL (without `/v1` — Claude Code adds it) |
| `ANTHROPIC_API_KEY` | Your proxy API key |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Model for the Sonnet slot (default/primary model) |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | Model for the Opus slot (large/complex tasks) |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Model for the Haiku slot (fast/simple tasks) |
| `*_SUPPORTED_CAPABILITIES` | `"thinking,interleaved_thinking"` to enable extended thinking display |
| `DISABLE_PROMPT_CACHING` | Set to `"1"` for non-Anthropic backends |
| `CLAUDE_CODE_DISABLE_1M_CONTEXT` | Set to `"1"` to avoid 1M context requests |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | Set to `"1"` to reduce extraneous API calls |
| `API_TIMEOUT_MS` | Request timeout (default 900000 = 15 minutes) |

## Model selection

Claude Code has three model slots. Each can be mapped to any model in your proxy:

- **Sonnet** — the default model used for most tasks
- **Opus** — used for complex reasoning (selected with `/model opus`)
- **Haiku** — used for fast, simple tasks (selected with `/model haiku`)

All three can point to the same model if you only have one.

## Thinking / reasoning support

For **translated backends** (non-Anthropic): if the model emits reasoning tokens (like MiniMax-M2.5), the proxy converts them to Anthropic thinking blocks that appear in Claude Code's output. These use placeholder signatures — Claude Code stores them and passes them back, but they never reach a real Anthropic API for validation. On subsequent turns, the proxy strips thinking blocks before sending to the Chat Completions backend.

Set `*_SUPPORTED_CAPABILITIES` to `"thinking,interleaved_thinking"` so Claude Code displays the thinking content. Leave empty for models that don't emit reasoning tokens.

For **native Anthropic backends**: real extended thinking with cryptographic signatures works normally through passthrough.

### Effort control for translated backends

Claude Code's `/effort` command (and the `thinking.budget_tokens` it sends) is honored by the proxy for translated backends that understand the DeepSeek thinking protocol:

- **Official DeepSeek models** (`deepseek-v4-flash` / `deepseek-v4-pro`) get the full injection automatically: the proxy maps the client's `thinking` budget to `reasoning_effort` and forwards `thinking: {type: enabled/disabled}` to the backend.
- **Third-party DeepSeek-compatible gateways** (e.g. a gateway in front of a DeepSeek model) get the same full injection when the model sets `thinking_passthrough: true` in the proxy config — see [config-reference.md](config-reference.md).

The client's effort maps to DeepSeek's tiers as follows (Claude Code `medium` collapses to `high`, since DeepSeek has no medium tier):

| Claude Code effort | `budget_tokens` | DeepSeek `reasoning_effort` |
|---|---|---|
| low | 1024 | low |
| medium | 8192 | high |
| high | 20480 | high |
| xhigh | 32768 | xhigh |
| max | 64000 | max |

For **other OpenAI-compatible backends** that don't support the DeepSeek `thinking` key, the proxy forwards only the OpenAI-standard `reasoning_effort` field — which they either honor or ignore — and never sends the DeepSeek-only `thinking` parameter.

**Note:** If you use a Claude Code optimizer that injects `thinking: {type: "adaptive"}` (no fixed budget), the proxy maps it conservatively to `high` (DeepSeek's default), since the effort cannot be determined from a dynamic budget.

## Web search

Claude Code's built-in `WebSearch` tool (`web_search_20250305`) is an Anthropic server-side feature. It works with native Anthropic backends through passthrough.

For translated backends, the proxy can handle web search transparently using the processing pipeline:

**Option 1: Proxy-side search (recommended)** — Configure a Tavily API key in the proxy's `processors` block:

```yaml
processors:
  web_search_key: tvly-your-key
```

The proxy automatically converts Claude Code's `web_search_20250305` server tool to a function tool that the backend model can call. When the model calls `web_search`, the proxy executes the Tavily search and injects the results — transparent to Claude Code. No client-side MCP configuration needed.

**Option 2: Client-side MCP** — Configure [Tavily](https://tavily.com/) as an MCP server in Claude Code's settings. The config generator can set this up — enter your Tavily API key and the generated config will include the MCP setup command.

## Image handling

The proxy's processing pipeline can handle images for text-only backends:

**Vision-capable backends** (`supports_vision: true`): Images pass through the translation normally.

**Text-only backends with a vision processor configured**: The proxy sends each image to the vision model for description, then replaces the image with the text description. The backend model receives only text. Configure this in the proxy:

```yaml
processors:
  vision: qwen-3.5    # any vision-capable model in your config

models:
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1
    # Images auto-routed to qwen-3.5 for description

  - name: qwen-3.5
    backend: http://192.168.13.30:8000/v1
    supports_vision: true    # handles images natively, no processing needed
```

**Text-only backends without a vision processor**: The proxy returns a clear error message: *"The backend model does not appear to support image inputs."* with configuration guidance.

## Proxy-side config

On the proxy side, no special model configuration is needed. Any model in your `config.yaml` is automatically available to Claude Code. The proxy detects whether the backend speaks Anthropic or OpenAI protocol and translates accordingly.

```yaml
models:
  # OpenAI backend — proxy translates Messages → Chat Completions automatically
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1

  # Anthropic backend — proxy passes through natively
  - name: claude-sonnet-4
    backend: https://api.anthropic.com
    type: anthropic
    api_key: sk-ant-...
```

## Recommended hybrid setup

A common pattern is to keep the **default slots on translated, text-only backends** (cheap, controllable, images/PDFs handled by the vision processor) while reserving **one slot for a native Anthropic multimodal backend** (e.g. an Anthropic-compatible vision model served by a gateway like Alibaba Cloud Bailian MaaS) for tasks that genuinely need the model to *see* the image itself.

```yaml
models:
  # Default slots: text-only LLM + vision processor handles images/PDFs
  - name: MiniMax-M2.5
    backend: http://192.168.100.10:8000/v1

  # "Multimodal" slot: native Anthropic backend, full-fidelity passthrough.
  # Pick this slot only when the raw image matters (charts, diagrams, mixed
  # text+image reasoning) — the translation path's description loses nuance.
  - name: qwen3.7-plus
    backend: https://ws-<...>.maas.aliyuncs.com/apps/anthropic
    type: anthropic
    api_key: sk-...
```

Client side (Claude Code / CC switch):

```
ANTHROPIC_DEFAULT_SONNET_MODEL="MiniMax-M2.5"   # text-only + vision pipeline
ANTHROPIC_DEFAULT_OPUS_MODEL="MiniMax-M2.5"
ANTHROPIC_DEFAULT_HAIKU_MODEL="MiniMax-M2.5"
ANTHROPIC_DEFAULT_<FABLE>_MODEL="qwen3.7-plus"  # native multimodal, on demand
```

What the hybrid buys you:

- **Default work stays cheap**: text-only backends get images/PDFs via the vision processor pipeline — no per-request multimodal cost.
- **Multimodal fidelity on demand**: the native slot passes the raw request through untouched, so the model sees original images and you get real thinking signatures and real prompt caching (if the backend implements the Anthropic protocol faithfully) instead of the translation path's placeholder signatures.
- **Slots are independent**: each slot maps to its own model; switching does not affect the others.

Notes:

- `type: anthropic` backends skip the pipeline entirely unless `force_pipeline: true` is set, so nothing in the request is rewritten or cropped. A PDF sent to the native slot passes through as an Anthropic `document` block — use that slot only if the backend handles documents/images natively.
- `DISABLE_PROMPT_CACHING` is a global Claude Code env var, not per-slot. If set to `"1"` for the translated slots, it also disables caching for the native slot. Remove it if you want the native backend to receive `cache_control` markers (harmless for translated backends, which ignore them).

## Known limitations (translated backends)

- **Extended thinking**: Reasoning tokens from the backend are displayed as thinking blocks, but they don't have real Anthropic signatures. This is cosmetic — tool calling and agentic behavior work normally.
- **Prompt caching**: Stripped silently. All translated requests are uncached.
- **Server-side web search**: Not available directly, but the proxy can execute web searches via Tavily when `web_search_key` is configured in the `processors` block. Alternatively, use Tavily MCP.
- **Image support**: Text-only models work with images when a vision processor is configured. Otherwise, the proxy returns a clear error with configuration guidance.
- **PDF support**: The proxy extracts PDFs via the **MinerU cloud API** (`processors.mineru_api_key`) — structure-preserving markdown with embedded images described by the vision model — so PDFs work for text-only backends. Without a MinerU key, PDF blocks become a failure placeholder.

Native Anthropic backends have full fidelity — all features work through passthrough. Use `force_pipeline: true` on an Anthropic model to override and use proxy-side processing instead.
