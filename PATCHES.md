# go-llm-proxy 定制补丁记录

基于 **go-llm-proxy v0.3.9**（官方源码）的本地定制版本。
用途：让 Claude Code + CC Switch → go-llm-proxy → **DeepSeek（OpenAI 协议，thinking 模式）** 链路正常工作。

## 补丁清单（共 5 个）

### 补丁 1：thinking 块回填 `reasoning_content`
**文件**：`internal/handler/messages_translate.go` — `translateAssistantMessage`
**问题**：DeepSeek thinking 模式要求 `reasoning_content` 原样回传，否则报 400
`reasoning_content must be passed back`。go-llm-proxy 翻译 Anthropic assistant 消息时把 thinking 块丢弃了。

**修复**：
- 新增 `reasoningParts []string`，`thinking`/`redacted_thinking` case 捕获文本并追加。
- 翻译后设置 `msg["reasoning_content"]`（无 thinking 时为空字符串）。
- `content` 使用 `""` 而非 `nil`，保证 JSON 里始终有内容字段。

### 补丁 2：工具消息排在 user text 之前
**文件**：`internal/handler/messages_translate.go` — `translateUserMessage`
**问题**：Claude Code 会把 text + tool_result 合并到同一条 user 消息里，翻译后若 user text 在 tool 消息之前，则 OpenAI 协议中 `tool_calls → tool` 不再相邻，报 400
`insufficient tool messages following tool_calls`。

**修复**：把 `result = append(result, toolResults...)` 移到 `userParts` 之前——先发 tool 消息，再发 user text。

### 补丁 3：非流式 usage 语义对齐 Anthropic
**文件**：`internal/handler/messages.go`（~line 455）
**问题**：官方 Anthropic 语义下 `input_tokens` 排除缓存命中，命中量单独放 `cache_read_input_tokens`。go-llm-proxy 直接把含命中的 `prompt_tokens` 塞进 `input_tokens`，导致监控面板前面的值虚高（混入命中量）。

**修复**：
```go
cacheRead := 0
if chatResp.Usage.PromptTokensDetails != nil {
    cacheRead = chatResp.Usage.PromptTokensDetails.CachedTokens
}
inputTokens := chatResp.Usage.PromptTokens - cacheRead
if inputTokens < 0 {
    inputTokens = 0
}
usageObj = map[string]any{
    "input_tokens":                inputTokens,
    "output_tokens":               chatResp.Usage.CompletionTokens,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens":     cacheRead,
}
```

### 补丁 4：流式 usage 语义对齐 + 真实缓存透传
**文件**：`internal/handler/messages_streaming.go`
**问题**：流式路径 `message_start`/`message_delta` 同样需要 `input_tokens` 排除缓存命中（`input = prompt - cached`，带下限保护），并让 `cache_read_input_tokens` 透传真实命中量，CC Switch 监控面板才能显示正确命中。

**修复**：
- `emitMessageStart`（~line 62）：从 `usageData.PromptTokensDetails.CachedTokens` 提取 `cacheRead`，`inputTokens := usageData.PromptTokens - cacheRead`（下限 0）。
- 主循环：`if chunk.Usage != nil { usageData = chunk.Usage }` 移到 `emitMessageStart` 之前（流式下 usage 只在最后一个 chunk 到达，须先于 `message_start` 赋值）。
- `message_delta`（~line 487）：`inputTokens = usageData.PromptTokens` 后 `-= usageData.PromptTokensDetails.CachedTokens`（下限 0），`cacheRead` 透传为 `cache_read_input_tokens`。

## 配套改动（补丁 3/4 依赖）
**文件**：`internal/api/types.go`
- `ChunkUsage.PromptTokensDetails *PromptTokensDetails`（`json:"prompt_tokens_details,omitempty"`）
- `PromptTokensDetails.CachedTokens int`（`json:"cached_tokens,omitempty"`）
- `ChatChoiceMsg.EffectiveReasoning()` / `ChunkDelta.EffectiveReasoning()`：兼容返回 `reasoning` 或 `reasoning_content` 字段。

## 验证结果
- 400 错误全部消除（thinking 回填 + 工具消息重排）。
- vision 链路通过 CC Switch 验证（流式 `"这张图是红色。"`）。
- 缓存命中透传验证：`input_tokens: 45` + `cache_read_input_tokens: 3840` = 总 prompt 3885，与官方语义一致。

## 构建与运行（Windows）
```bash
# 编译（外部网络需走代理）
HTTPS_PROXY=http://127.0.0.1:1079 HTTP_PROXY=http://127.0.0.1:1079 \
  GOROOT=C:\Users\Peter\go-dist\go \
  go build -o go-llm-proxy.exe .
# 运行
start-proxy.bat
```
注意：`config.yaml` 含 API 密钥，已被 `.gitignore` 忽略，不入库。

## 补丁 5：视觉链路超时与缓存修复
**文件**：`internal/pipeline/vision.go`、`internal/pipeline/cache.go`、`internal/pipeline/pipeline.go`、`internal/config/config.go`
**问题**：两条视觉链路缺陷——
1. 视觉模型调用走通用 `http.Client`（`ResponseHeaderTimeout: 30s`），慢视觉模型（大图、并发、历史重放）请求超时，图片被降级为 `[Image could not be processed]`；
2. 图片缓存 `maxCacheEntries=1024` 满时**整表清空**，Claude Code 每轮重放历史消息，缓存清空后所有历史图片被迫重新走视觉模型。

**修复**：
- 新增独立视觉 client（`newVisionClient`），`ResponseHeaderTimeout: 180s`（`visionResponseHeaderTimeout` 常量），`describeImage` 改用 `p.visionClient`（fallback 到 `p.client` 保持测试兼容）。图片和 PDF 两条路径都受益。
- `visionCtx` 60s→120s，作为外层总限（180s 是响应头限，120s 是总限）。
- 缓存淘汰策略从「满则全清」改为 **LRU**（`boundedCache` 用单调递增 `seq` 淘汰最久未用条目，避免时间戳碰撞）；`Load` 刷新 recency，修正竞态。
- `max_images_per_request` 从硬编码常量改为可配置（`ProcessorsConfig.MaxImagesPerRequest`，默认 10），默认常量改名 `defaultMaxImagesPerRequest`。

**配套**：`processImages` 签名加 `maxImagesPerRequest` 参数；`docs/pipeline.md`、`docs/config-reference.md`、`README.md`、`config.yaml.example` 同步更新文档。

**验证结果**：
- `go build ./...`、`go vet ./...`、`go test ./...` 全包通过（pipeline 测试更新 eviction 用例以匹配 LRU 语义，14 处 `processImages` 调用签名同步）。
- 重启后 8 个模型 `health: model online`，端到端请求返回正常。
- 新配置项 YAML 绑定解析验证通过。
