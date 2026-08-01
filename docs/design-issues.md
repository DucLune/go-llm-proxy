# go-llm-proxy 设计问题分析报告

> 审查范围：`/v1/messages` 请求处理链路（协议翻译 → Pipeline → 后端转发 → 响应回译）
> 重点关注：纯文本后端 + 视觉辅助模型的使用模式

---

## P0 — 高优先级

### 1. 视觉模型无熔断机制

**位置**: `internal/pipeline/vision.go` — `processImages()`

**问题**: 当视觉模型后端宕机或不可达时，没有 circuit breaker 快速失败。每个带图请求都会尝试所有图片的视觉调用，等待超时后才失败。

**影响**:
- 配置 `max_images_per_request: 100` + `maxConcurrentVision = 5` 时，最坏情况 100/5 × 180s = 3600s 等待
- 大量 goroutine 堆积（每个请求 100 个 vision goroutine）
- 所有用户请求被阻塞

**建议**: 引入简单的熔断器（连续 N 次失败后短路一段时间），或在 `processImages` 入口检查视觉模型健康状态（已有 `HealthStore`）。

---

### 2. 图片数量与请求超时冲突

**位置**: `internal/handler/messages.go` L115, `internal/pipeline/vision.go`

**问题**: 请求上下文使用 `model.Timeout`（默认 300s），但视觉处理可能远超此时限。

```
请求上下文: model.Timeout = 300s
视觉客户端: ResponseHeaderTimeout = 180s (visionResponseHeaderTimeout)
最大图片数: max_images_per_request = 100
并发上限:   maxConcurrentVision = 5
最坏耗时:   100 / 5 × 180s = 3600s >> 300s
```

**影响**: 图片较多时，context 取消会杀死所有进行中的视觉调用，用户得到部分失败或不完整的描述。`runPipelineWithKeepalives` 只防止客户端超时，不延长服务端上下文。

**建议**:
- Pipeline 处理应使用独立于 `model.Timeout` 的上下文（或取两者较大值）
- 或在 Pipeline 内部设置独立的总处理超时
- 减少 `max_images_per_request` 到合理值（如 30）

---

## P1 — 中优先级

### 3. 用户图片失败无 TTL 缓存

**位置**: `internal/pipeline/vision.go` L282-308

**问题**: tool-role 图片失败会写入 `failKey`（5 分钟 TTL），但 user-role 图片失败**完全不缓存**。

```go
// 只有 tool-role 才走这里（failKey 非空）
if jobs[i].failKey != "" {
    imageCache.StoreWithTTL(jobs[i].failKey, "1", imageFailureTTL)
}
// user-role 的 imageJob 没有设置 failKey，失败后无任何缓存
```

**影响**: Claude Code 每轮重放完整对话历史。视觉模型不稳定时，所有历史用户图片**每轮都重试**，反复轰炸已故障的后端。

**建议**: 为 user-role 图片也设置 `failKey`，使用相同的 5 分钟 TTL。

---

### 4. 流式搜索只支持单轮

**位置**: `internal/handler/messages_streaming.go` L345-434

**问题**: 非流式路径有 `HandleNonStreamingSearchLoop`（最多 5 轮迭代），但流式路径只执行一次搜索。如果后端在收到搜索结果后再次调用 `web_search`，第二次调用会作为普通 `tool_use` 块直接发给客户端。

```
流式: 后端调用 web_search → 代理执行搜索 → 重新请求后端
     → 后端再次调用 web_search → 作为 tool_use 发给客户端（未处理）

非流式: 循环最多 5 次，直到后端不再调用 web_search
```

**影响**: 多轮搜索场景下，客户端收到意外的 `tool_use` 块，Claude Code 可能尝试本地执行一个不存在的工具。

**建议**: 流式路径也实现搜索循环（至少 2-3 轮），或在 `streamFromBackend` 返回后检查是否有新的搜索调用。

---

### 5. `headersAlreadySent` 的尴尬状态

**位置**: `internal/handler/messages.go` L148-174

**问题**: 流式 + 有图片时，`runPipelineWithKeepalives` 会先发送 HTTP 200 + SSE 头（为了防止客户端超时），然后 Pipeline 才开始处理。如果 Pipeline 随后失败：

```
客户端收到: HTTP 200 OK → event: error\ndata: {...}\n\n → 连接关闭
```

而不是一个干净的 HTTP 500 错误。

**影响**: 部分客户端（尤其是非 Claude Code 的自定义客户端）可能不能正确处理"200 后跟 error 事件"的模式，可能将其视为空响应或解析错误。

**建议**: 这是 SSE 协议的固有限制，难以完美解决。可以考虑：
- 在 keepalive 阶段只发 SSE 注释（`: keepalive`），不发送 200 状态码
- 或文档中明确说明此行为

---

## P2 — 低优先级

### 6. tool_result 顺序倒置

**位置**: `internal/handler/messages_translate.go` L184

**问题**: 翻译时 tool_result 被放在 user text 之前：

```go
result = append(result, toolResults...)  // tool messages 先
result = append(result, userMessage)     // user text 后
```

原始 Anthropic 消息可能是 `[text, tool_result]`，翻译后变成 `[tool, tool, user_text]`。

**原因**: 注释说明这是为了满足 OpenAI 格式要求（role:tool 必须紧跟 assistant 的 tool_calls）。

**影响**: 语义顺序被颠倒，可能影响部分模型对上下文的理解。对 DeepSeek 等模型影响较小（它们通常能处理），但对顺序敏感的模型可能有影响。

---

### 7. 搜索工具注入改变 tool_choice

**位置**: `internal/pipeline/search.go` L130-132

**问题**:

```go
if _, hasChoice := chatReq["tool_choice"]; !hasChoice {
    chatReq["tool_choice"] = "auto"
}
```

原本没有 tools 的请求，注入搜索工具后强制设了 `tool_choice: "auto"`。

**影响**: 可能导致后端在不该调用工具时调用工具（虽然概率较低，因为模型通常只在需要时调用）。

---

### 8. 图片 + PDF 处理串行

**位置**: `internal/pipeline/pipeline.go` L186-210

**问题**:

```go
chatReq, err = p.processImages(...)  // 先处理完所有图片
chatReq, err = p.processPDFs(...)    // 再处理 PDF
```

两者无数据依赖（操作不同的 content parts），完全可以并行。

**影响**: 同时包含图片和 PDF 的请求，延迟叠加。实际场景中 PDF 页面图片走 OCR 路径，与用户图片走 vision 路径，互不干扰。

---

### 9. 流式响应无总时长限制

**位置**: `internal/handler/stream_timeout.go`

**问题**: 一旦流式开始，超时被 disarm：

```go
func requestTimeout(parent context.Context, seconds int) (ctx, cancel, disarm) {
    ctx, cancel = context.WithCancel(parent)
    t := time.AfterFunc(time.Duration(seconds)*time.Second, cancel)
    return ctx, cancel, func() { t.Stop() }  // 流式开始后调用
}
```

唯一约束是客户端断开或进程关闭。

**影响**: 一个每 29 秒发一个字节的后端可以无限占用连接和 goroutine。

**建议**: 考虑添加一个更宽松的空闲超时（如 5 分钟无数据则断开），而非完全取消限制。

---

### 10. LRU 驱逐 O(n) 扫描

**位置**: `internal/pipeline/cache.go` L91-100

**问题**: `evictOne()` 遍历全部 1024 条找最旧的。

**影响**: 当前规模（1024 条）下不是瓶颈（微秒级），但设计上不够优雅。标准做法是双向链表 + map（O(1) 驱逐）。

---

### 11. 配置校验缺失：`messages_mode: translate` + `type: anthropic`

**位置**: `internal/config/config.go` — `validateConfig()`

**问题**: 如果在 anthropic 后端上强制 `messages_mode: translate`，翻译后的请求发到：

```
https://api.anthropic.com/chat/completions  ← 不存在的路径
```

没有配置校验警告，运行时直接 404。

**建议**: 在 `validateConfig` 中检测此组合并输出警告日志。

---

### 12. 翻译有损

**位置**: `internal/handler/messages_translate.go`

| 丢失的信息 | 影响 |
|---|---|
| `cache_control` 字段 | 后端无法利用 prompt caching |
| 多段 system 合并为单字符串 | 丢失结构（对 DeepSeek 无影响） |
| 历史中的 `server_tool_use` → 纯文本 | 后端丢失结构化搜索上下文 |
| user 消息中的 thinking block | 静默丢弃（正确行为，但不可逆） |

**影响**: 对于纯文本后端（DeepSeek），这些丢失基本无影响。但如果未来需要支持 prompt caching 或更复杂的工具链，需要重新设计翻译层。

---

## 架构层面的观察

### 正面设计

- **Pipeline 与翻译层解耦**: Pipeline 操作 Chat Completions 格式，不关心原始协议
- **缓存 key 包含模型名+prompt**: 切换视觉模型后自动失效旧缓存
- **SSRF 防护**: 图片 URL 有 preflight 检查 + 安全 HTTP 客户端
- **搜索等待有优雅断开**: `waitForSearchOrDisconnect` 处理了客户端中途断开的情况
- **think tag 流式过滤**: 正确处理了跨 chunk 边界的 `<think>` 标签

### 潜在改进方向

- 视觉处理结果可以考虑持久化缓存（当前仅内存，重启丢失）
- Pipeline 处理进度可以通过 SSE 事件通知客户端（当前只有 keepalive 注释）
- 多视觉模型负载均衡（当前只支持单一 vision 模型配置）
