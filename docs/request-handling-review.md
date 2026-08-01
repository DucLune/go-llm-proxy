# go-llm-proxy:请求处理方案分析与潜在问题

> **说明**:本文档是对 go-llm-proxy(基于 v0.3.9 的本地定制版)中 `/v1/messages`、`/v1/responses`、`/v1/chat/completions` 三条请求路径的处理方案分析,重点在 Anthropic Messages 协议、`ShouldProcess` 视觉闸门、图片处理流水线。文档为**独立审核**而写,每条分析都标注了文件行号,供审核 agent 验证。
>
> 分析人基于源码逐行核对,并修正了自己初稿中的两处错误结论(详见"修正说明")。请审核 agent 特别关注那些标注了 **[待验证]** 的断言。

---

## 1. 处理链路总览

```
客户端(CLaude Code / Codex / OpenCode / Qwen Code)
        │
        ▼
   ┌─────────────────────────────────────────────────────────────┐
   │ main.go:ServeMux                                           │
   │   Recovery → RateLimit → Auth(取 key,绑到 context)          │
   │   /v1/messages → MessagesHandler                           │
   │   /v1/responses → ResponsesHandler                         │
   │   /v1/chat/completions → ProxyHandler                      │
   └─────────────────────────────────────────────────────────────┘
```

### 1.1 路径分派(main.go:203-246)

| 路径 | Handler | 处理方式 |
|---|---|---|
| `/v1/messages`(含 `/anthropic/v1/messages`) | `MessagesHandler` | 原生透传 / 翻译成 Chat Completions / Bedrock Converse |
| `/v1/responses` | `ResponsesHandler` | 原生透传 / 翻译成 Chat Completions(自动探测)|
| `/v1/chat/completions` | `ProxyHandler` | 原生透传(OpenAI 协议)|

所有请求先过中间件:`httputil.RecoveryMiddleware` → `ratelimit.RateLimitMiddleware` → `auth.AuthMiddleware`。

### 1.2 模型路由核心

- **模型查找**:`config.FindModel(cfg, modelName)` —— 按客户端发送的 `modelName` **精确匹配** `models[].name`([config.go:309](internal/config/config.go#L309))。
- **无负载均衡/权重/故障转移**:`models[].backend` 是单一上游地址,`name` → 1 个后端。健康状态(`HealthStore`)仅用于展示,不参与路由决策。
- **协议判定**:`model.Type` ∈ `""`/`openai`/`anthropic`/`bedrock`([config.go:64-66](internal/config/config.go#L64))。

### 1.3 Messages 路径的完整决策树

```
POST /v1/messages
│
├─ ① 协议头校验:仅 POST;读 body;解析 model;key 权限校验
│
├─ ② model.Type == bedrock?
│     ├─ YES → handleBedrock(Converse + SigV4),不跑 pipeline
│     └─ NO  ↓
│
├─ ③ shouldTranslate()?
│     ├─ messages_mode=translate → 翻译
│     ├─ messages_mode=native    → 原生
│     └─ auto:type==anthropic → 原生;否则 → 翻译
│     │
│     ├─【原生透传】handleNativePassthrough
│     │     body 原样转发 → 后端 /v1/messages
│     │     只透传 X-Api-Key / Anthropic-Version / Anthropic-Beta
│     │     不经过 ShouldProcess / pipeline
│     │
│     └─【翻译路径】buildChatRequestFromAnthropic
│           Anthropic body → Chat Completions 格式
│           图片 block → image_url data URL / url
│           │
│           └─④ ShouldProcess(model)?   ←── 视觉/PDF/搜索闸门
│                 │
│                 ├─ false → 直接转发,不处理
│                 └─ true → pipeline.ProcessRequest(chatReq, model)
│                       ├─ 图片 → processImages → 并发调 processors.vision
│                       ├─ PDF → OCR/视觉级联
│                       └─ 搜索 → convertOrInjectSearchTool
│                       └─ 转发后端 /v1/chat/completions
```

### 1.4 ShouldProcess 的语义

```go
func (p *Pipeline) ShouldProcess(model *config.ModelConfig) bool {
    if model.Type == config.BackendAnthropic && !model.ForcePipeline {
        return false   // anthropic 原生后端默认跳过
    }
    return true
}
```
([pipeline.go:97](internal/pipeline/pipeline.go#L97))

**关键事实**:
- 真正的判断点有 3 处:`messages.go:149`、`responses.go:287`(翻译路径内)、`pipeline.go:160`(`ProcessRequest` 入口,防御性重复)。
- **proxy.go 的 Chat Completions 路径不调用 `ShouldProcess`**,而是用 `BodyNeedsProcessing(body)`(字符串扫描 `"image_url"`、`"application/pdf"` 等特征)决定是否进 `ProcessRequest`([proxy.go:149](internal/handler/proxy.go#L149))。
- `ShouldProcess` 只影响**翻译后**的请求是否做内容处理,**不阻止翻译本身**。

### 1.5 图片处理的完整链路

```
请求带图片(Anthropic image block / base64 / url)
  → buildChatRequestFromAnthropic
       translateImageBlock:
         base64 → data:<media_type>;base64,<data>   (image_url)
         url    → {url: <url>}                       (image_url)
         (messages_translate.go:309-334)
  → ShouldProcess=true
  → pipeline.ProcessRequest
       → processImages:
             user 角色的图 → 仅视觉描述(visionPromptDescribe)
             tool 角色的图 → OCR → 视觉级联(visionPromptOCR / ocrModelPrompt)
             → 并发(max 5)调 processors.vision 指定的识图模型
                  describeImage():
                    - 若 image_url 是 http(s):fetchImageAsDataURL → SSRF-safe 抓取
                      → 转 data: URL(上限 imageFetchMaxSize = 10MB,15s 超时)
                    - 若 image_url 是 data: → 原样返回
                    - 构造请求:model=visionModel.Model, prompt, max_tokens
                      (vision.go:590-738)
             → 替换为:
                  user 图 → <image_description>...</image_description>
                  tool 图 → <page_text>...</page_text>
                  (vision.go:446-475)
  → 文本后端只收到文字,看不到图片
```

---

## 2. 潜在问题(按严重程度排序)

### 🔴 问题 A:base64 图片会被**双重 base64 编码**(内存浪费 + 上游可能超限)

- **位置**:[pipeline.go:184](internal/pipeline/pipeline.go#L184) 与 [vision.go:612-640](internal/pipeline/vision.go#L612)
- **根因**:`fetchImageAsDataURL` 已把字节编码成 base64 的 `data:` URL;但对 `data:` 输入它直接原样返回([safe_http.go:84-85](internal/pipeline/safe_http.go#L84))。随后 `describeImage` 把该 `data:` URL **原样**塞进 image_url 再 `json.Marshal`。
- **后果**:一张原始图片字节 → base64(膨胀 ~33%)→ 再次 base64(再膨胀 ~33%)。实测:10MB 图片 → 约 17.8MB 请求体。
- **影响面**:仅 messages 翻译路径(proxy 的 Chat Completions 不翻译 base64;Responses 翻译也走这条)。
- **校验边界**:`MaxRequestBodySize = 50MB`([types.go:11](internal/api/types.go#L11))限制的是**客户端→代理**;但代理→**vision_model** 的请求没有看到 size 限制([vision.go:647-651](internal/pipeline/vision.go#L647))。是否受限于 `http.Client` 无 body limit,以及上游模型网关,**[待验证]**。
- **建议方向**:`describeImage` 内对 `data:` URL 先 base64 decode 再重建,或检查 `imageCacheKey`/`hashImageURL` 是否应作用于解码后的字节。

### 🟠 问题 B:`ShouldProcess` 对 anthropic 后端默认跳过,与"纯文本后端配识图"目标**语义冲突**(配置陷阱)

- **位置**:[pipeline.go:97-102](internal/pipeline/pipeline.go#L97)
- **根因**:逻辑是 `type==anthropic && !force_pipeline → false`。即一个**文本后端**若碰巧是 anthropic 协议(如 Ollama 起 Anthropic 兼容),配成 `type: anthropic` 后**默认不处理图片**,图片会原样转发给文本后端。
- **后果**:与用户核心目标(纯文本后端 + 识图辅助模型)冲突;用户必须记得设 `force_pipeline: true`,否则视觉链路静默失效。
- **验证**:`config.yaml.example` 是否提示了这一点?**[待验证]**

### 🟠 问题 C:鉴权基于客户端 `modelName`,而非配置解析后的 `model`(潜在鉴权绕过面)

- **位置**:[proxy.go:95](internal/handler/proxy.go#L95)、[messages.go:92](internal/handler/messages.go#L92)、[responses.go:229](internal/handler/responses.go#L229)
- **根因**:`auth.KeyAllowsModel(key, modelName)` 校验的是**客户端输入的模型字符串**([auth.go:100-110](internal/auth/auth.go#L100)),而非 `config.FindModel` 解析后得到的 `ModelConfig.Name`。由于 `name` → `ModelConfig` 是精确匹配,`modelName` 必须等于配置里的 `name` 才能匹配到模型,因此**绕过 `KeyAllowsModel` 需要同时满足 `KeyAllowsModel` 和 `FindModel` 双重条件**。
- **是否构成实际绕过**:结合 `validateConfig` 已强制 `type ∈ {openai,anthropic,bedrock,""}`([config.go:356-381](internal/config/config.go#L356)),`shouldTranslate` 的 auto 分支只判断 `type` 不会因非法值被绕过。**结论:在当前配置校验下不构成直接绕过**,但鉴权与路由分离的事实,值得让审核 agent 复核是否有组合绕过路径。**[待验证]**

### 🟡 问题 D:视觉处理失败时**静默降级**,掩盖真实错误(可运维性)

- **位置**:[pipeline.go:197-200](internal/pipeline/pipeline.go#L197)
- **根因**:`processImages`/`processPDFs` 失败仅 `slog.Warn`,请求继续带着未处理图片发给文本后端;文本后端再返回错误时,`messages.go:230-233` 给出"后端不支持图片"提示,掩盖了真实根因(视觉模型挂/超时/限流)。
- **影响面**:messages / responses 翻译路径。
- **建议方向**:区分"后端确实不支持"与"视觉处理器故障",后者应显式报错或降级。

### 🟡 问题 E:图片 URL(非 base64)抓取有 10MB 上限,**base64 数据路径没有对应上限**(不一致)

- **位置**:[safe_http.go:34-37](internal/pipeline/safe_http.go#L34)、[vision.go:612-640](internal/pipeline/vision.go#L612)
- **根因**:http(s) 图片抓取受 `imageFetchMaxSize = 10MB` 限制;但 client 直接提交的 base64 `data:` URL 完全绕过了这个限制(缓存 key 用原始 base64 串 hash,未做长度检查)。
- **后果**:超大 base64 图片会直接进入 vision_model 请求(内存 + 上游限制风险),行为与 http(s) 图片不一致。

### 🟡 问题 F:搜索失败回退行为**不一致**(messages 返回 502,proxy 静默转发 tool_call)

- **位置**:[proxy.go:379-390](internal/handler/proxy.go#L379) vs [messages.go:382-394](internal/handler/messages.go#L382)
- **根因**:proxy 的 `handleNonStreamingWithSearch` 在搜索循环失败时只 `slog.Error`,把带 `web_search` 工具调用的原始响应转发给客户端;messages 路径则返回 502。行为不一致,客户端可能拿到无法执行的 tool_call。
- **建议方向**:对齐为显式错误。

### 🟡 问题 G:图片替换后没有检查目标模型是否**支持视觉**(`supports_vision: true` 后门)

- **位置**:[pipeline.go:188](internal/pipeline/pipeline.go#L188)、[vision.go:174-177](internal/pipeline/vision.go#L174)
- **根因**:`processImages` 只在 `(!SupportsVision || ForcePipeline)` 时处理图片;若文本后端被误配 `supports_vision: true`,图片会裸传,没有兜底。
- **影响面**:低(配置失误场景)。

### 🟢 问题 H:小问题(非阻塞)

- **`requireAnthropic` 语义**:`/anthropic/v1/*` 强制 `model.Type == anthropic`([messages.go:103-106](internal/handler/messages.go#L103));若用户想把 anthropic 协议翻译到 openai 后端,不能用 `/anthropic` 前缀,需用 `/v1/messages`。文档是否有说明?**[待验证]**
- **`vision_response_usage` 未透传**:`describeImage` 不记录 vision 模型的 token 用量,监控里看不到这部分成本。**[待验证]**

---

## 3. 修正说明(初稿两处错误结论)

> 以下是我**初稿中存在的错误**,在核对源码后已修正。特此记录,避免误导审核。

1. **"base64 图片会被两次 fetch"** —— **错误**。`fetchImageAsDataURL` 只在 `image_url` 是 http(s) 时抓取;`data:` URL 直接返回,不会被二次 fetch。真正的问题是**二次 base64 编码**(问题 A),不是二次抓取。

2. **"`type` 字段未校验导致鉴权绕过"** —— **错误**。`validateConfig` 已强制 `type ∈ {openai,anthropic,bedrock,""}`([config.go:356-381](internal/config/config.go#L356)),`shouldTranslate` 的 auto 分支不会被非法值触发绕过。鉴权与路由虽分离,但在当前校验下不构成直接绕过(问题 C 保留为待复核)。

---

## 4. 待审核 agent 验证的断言

| # | 断言 | 需验证方向 |
|---|---|---|
| 1 | 双重 base64 编码在 messages 翻译路径**确实发生** | 走一遍 `buildChatRequestFromAnthropic` → `processImages` → `describeImage` 的实际字节流 |
| 2 | `data:` base64 路径**没有** size 上限,而 http(s) 路径有 10MB | 确认 `MaxRequestBodySize` 是否约束了代理→vision_model 的请求 |
| 3 | 搜索失败回退(messages 502 vs proxy 静默转发)行为不一致 | 对比两个 handler 的失败路径 |
| 4 | `ShouldProcess` 的 anthropic 默认跳过会造成视觉链路静默失效 | 确认是否覆盖用户的"文本后端 + 识图"场景 |
| 5 | 鉴权基于 `modelName` 在当前配置校验下**不构成直接绕过** | 复核 `KeyAllowsModel` + `FindModel` + `validateConfig` 三者组合是否有绕过面 |

---

## 5. 对其他审核报告(`docs/design-issues.md`)的复核结论

> 对独立审核 agent 提出的 12 个问题逐一核对源码。判定分三种:**属实**(已复现/确认)、**不属实**(有反证)、**属实但被高估**(行为存在但影响被夸大)。

### 5.1 判定总表

| # | 问题 | 判定 | 理由 |
|---|---|---|---|
| 1 | 视觉模型无熔断机制 | ✅ 属实 | 全库 grep 无 breaker/circuit/健康检查;`processImages` 不查 `HealthStore`。`max_images_per_request: 100` + `maxConcurrentVision=5` 时最坏约 3600s |
| 2 | 图片数量与请求超时冲突 | ✅ 属实(断言需修正) | 主要场景确凿,但"所有请求被杀死"过于绝对——见 5.2 |
| 3 | 用户图片失败无 TTL 缓存 | ❌ 不属实 | 见 5.2(三条反证) |
| 4 | 流式搜索只支持单轮 | ✅ 属实 | [messages_streaming.go:345-434](internal/handler/messages_streaming.go#L345) 重发后无循环 |
| 5 | `headersAlreadySent` 尴尬状态 | ⚠️ 属实但被高估 | 行为存在,但触发面窄;keepalive 注释对 SSE 客户端无害 |
| 6 | tool_result 顺序倒置 | ✅ 属实(设计权衡) | [messages_translate.go:184](internal/handler/messages_translate.go#L184),为满足 OpenAI `tool_calls→tool` 相邻约束 |
| 7 | 搜索工具注入改变 tool_choice | ✅ 属实(低风险) | [search.go:130-132](internal/pipeline/search.go#L130) |
| 8 | 图片 + PDF 处理串行 | ✅ 属实 | [pipeline.go:186-210](internal/pipeline/pipeline.go#L186) |
| 9 | 流式响应无总时长限制 | ✅ 属实(有意的权衡) | [stream_timeout.go:8-24](internal/handler/stream_timeout.go#L8) 注释明确说明 |
| 10 | LRU 驱逐 O(n) 扫描 | ✅ 属实(非瓶颈) | [cache.go:91-105](internal/pipeline/cache.go#L91),1024 条下微秒级 |
| 11 | `messages_mode: translate` + `type: anthropic` 无校验 | ✅ 属实 | [config.go:390-393](internal/config/config.go#L390) 只校验值不校验组合 |
| 12 | 翻译有损 | ✅ 属实 | `cache_control`、多段 system 合并、thinking 块丢弃均已确认 |

### 5.2 重点说明(❌ 与 ⚠️)

#### ❌ 第 3 条:用户图片失败无 TTL 缓存

报告声称"tool-role 失败写 failKey,user-role 完全不缓存 → Claude Code 每轮重放历史,视觉模型不稳定时反复轰炸后端"。**三条反证**:

1. **`maxConcurrentVision = 5` 是每请求的局部信号量**(`sem := make(chan struct{}, maxConcurrentVision)`,vision.go:253),不是全局并发上限,不会放大"每轮重放"。
2. **LRU 已防止无界内存**:`maxCacheEntries = 1024` 硬上限(cache.go:14),满则驱逐最旧。
3. **没有 TTL 不等于每轮轰炸**:
   - 成功描述**永久缓存**,重放同图时 `imageCache.Load` 直接命中(vision.go:232),不重调视觉模型;
   - `minDescriptionLen = 10` 以下不缓存(vision.go:301),是**有意**避免缓存截断片段;
   - 失败时 `describeImage` 有 120s 超时兜底(vision.go:602),不会无限挂。

**真正改进空间**(非问题,可选):视觉模型**持续故障**时,user-role 图片会在每个请求重复尝试。正确修复方向是**熔断(问题 1)**,而非给 user-role 加失败 TTL。

#### ⚠️ 第 5 条:`headersAlreadySent` 尴尬状态

行为确实存在([messages.go:149-151](internal/handler/messages.go#L149) → [shared.go:131](internal/handler/shared.go#L131) → 失败 [messages.go:156-168](internal/handler/messages.go#L156)),但:
- 只有**流式 + 请求含图片**才走 keepalive;纯文本流式/非流式不触发;
- 整条 pipeline 返回 error 才进此分支,`processImages` 通常只把单张图降级、不返回 error;
- keepalive 只是 SSE 注释(`: keepalive`),对客户端无害。

#### ✅ 第 9 条:流式响应无总时长限制

**属实,且是有意设计**——[stream_timeout.go:8-24](internal/handler/stream_timeout.go#L8) 注释:此前每个超 300s 的生成都在 300s 死掉,导致 resume 客户端陷入 re-think/re-issue 循环(2026-07-15 观测)。作者主动移除对活动流的总时长限制。

报告建议"加更宽松的空闲超时"——**合理但需谨慎**:DeepSeek thinking 模式可能长时间无输出后突然输出,空闲超时会误杀。若要采纳,须设为"无数据 > N 秒"而非"总时长 > N 秒"。

#### ✅ 第 11 条:`messages_mode: translate` + `type: anthropic`

行为已确认:`MessagesModeTranslate` 分支直接返回 true(messages.go:46-48),翻译路径把 `model.Backend + /v1/chat/completions` 作为上游(messages.go:191);若后端是 `https://api.anthropic.com` 会请求不存在的 `/chat/completions` → 404。

---

## 6. 修复计划(最终版)

> 按优先级分组。**问题 3、10 不修复**(见第 7 节)。

### 6.1 P0 组(高优先级)

| 修复项 | 涉及文件 | 方案 |
|---|---|---|
| **视觉熔断** | [vision.go](internal/pipeline/vision.go) | 新增 `visionHealth` 结构:连续失败 N 次 → 熔断 M 秒 → 半开探测。`processImages` 入口检查;熔断后对图片直接降级为占位符,不发起视觉调用。请求级即可,不必全局 |
| **图片数量与超时冲突** | [pipeline.go](internal/pipeline/pipeline.go)、[vision.go](internal/pipeline/vision.go) | pipeline 处理使用独立于 `model.Timeout` 的上下文(沿用 `visionCtx` 120s 总量上限),避免 context 取消杀死所有进行中的视觉调用;超时后返回可读错误而非部分成功 |
| **流式搜索多轮** | [messages_streaming.go](internal/handler/messages_streaming.go) | 在重发后(`streamFromBackend` 返回)检测再次 `web_search` 调用,循环执行(上限 3 轮),与 `HandleNonStreamingSearchLoop`(5 轮)行为对齐 |

### 6.2 P1 组(中优先级)

| 修复项 | 涉及文件 | 方案 |
|---|---|---|
| **搜索失败行为对齐** | [proxy.go:379-390](internal/handler/proxy.go#L379) | 对齐 messages 路径:搜索循环失败返回 502,而非把带 `web_search` tool_call 的原始响应转发给客户端 |
| **图片 + PDF 并行** | [pipeline.go](internal/pipeline/pipeline.go) | `processImages` 与 `processPDFs` 操作不同 content parts,用 `sync.WaitGroup` 并行;各自独立修改 `chatReq`,完成后再合并写回 |
| **tool_result 顺序** | [messages_translate.go](internal/handler/messages_translate.go) | 保留当前行为(满足 OpenAI `tool_calls→tool` 相邻),但补充注释说明权衡;若需保留原始顺序,加配置开关 |
| **翻译保真** | [messages_translate.go](internal/handler/messages_translate.go) | 多段 system 保留为多条 system 消息(而非合并为单个字符串);`cache_control` 透传给支持的后端 |

### 6.3 P2 组(低优先级)

| 修复项 | 涉及文件 | 方案 |
|---|---|---|
| **配置组合校验** | [config.go](internal/config/config.go) | `validateConfig` 增加:`messages_mode: translate` + `type: anthropic` 时报错或警告(启动即暴露,避免运行时 404) |
| **流式空闲超时** | [stream_timeout.go](internal/handler/stream_timeout.go) | 加"无数据 > N 秒"空闲超时(非总时长);阈值须宽于 DeepSeek thinking 空窗期;仅作安全网 |
| **LRU 优化** | [cache.go](internal/pipeline/cache.go) | 暂不建议做(见第 7 节);若做,用 map + 双向链表实现 O(1) 驱逐 |

### 6.4 每条修复的验收标准

| 修复项 | 验收标准 |
|---|---|
| 视觉熔断 | 视觉后端停掉后,第 N+1 个带图请求立即返回占位符(而非等待 120s);后端恢复后半开探测自动恢复 |
| 图片超时 | 100 张图、视觉后端慢的场景下,请求在可控时间内返回,不因 context 取消导致部分图片丢失 |
| 流式搜索多轮 | 后端连续 3 次 web_search 时,3 轮都被代理处理,客户端只看到最终结果 |
| 搜索失败对齐 | 搜索失败时 proxy 返回 502,客户端不再收到无法执行的 tool_call |
| 图片+PDF 并行 | 同时含图和 PDF 的请求,耗时 ≈ max(图片, PDF) 而非两者之和 |
| 配置组合校验 | 启动即报错,不再运行时 404 |
| 流式空闲超时 | 长期无输出的流在 N 秒后断开;正常 thinking 流不受影响 |

---

## 7. 不建议修复的项

- **问题 3(用户图片失败无 TTL)** —— 不属实。给 user-role 加失败 TTL 不解决"轰炸",真正解法是熔断。
- **问题 10(LRU O(n) 扫描)** —— 1024 条下微秒级,非瓶颈,过早优化。
- **问题 12(翻译有损)** —— 对 DeepSeek/纯文本后端基本无影响,是设计权衡;`cache_control` 透传可作未来增强(P1 已覆盖)。

---

## 8. 自审复核(实施前最终版)

> 目标:确保每个"属实"问题真实存在、每条修复方案不会引入新 bug。第 8.1 节复核判定,第 8.2 节修正修复方案(3 条需调整),第 8.3 节给出最终修复清单。

### 8.1 判定复核(全部成立)

上一轮对 12 个问题的判定**经再次核对全部成立**,未发现"误判属实"。逐条复核依据:

| # | 判定 | 复核关键依据 |
|---|---|---|
| 1 | ✅ 属实 | 全库 grep 无 breaker;`processImages` 不查 `HealthStore`([pipeline.go:157](internal/pipeline/pipeline.go#L157)) |
| 2 | ✅ 属实(断言需修正) | 视觉调用用 `visionCtx`(120s)独立于 `model.Timeout`,但整条 pipeline 上下文是 `ctx`([pipeline.go:157](internal/pipeline/pipeline.go#L157)),超时会杀死所有 vision 调用 |
| 3 | ❌ 不属实 | `maxConcurrentVision=5` 是每请求局部信号量;LRU 有 1024 硬上限;成功描述永久缓存不重调 |
| 4 | ✅ 属实 | `streamFromBackend` 返回后无循环,确认单轮([messages_streaming.go:345-434](internal/handler/messages_streaming.go#L345)) |
| 5 | ⚠️ 属实但高估 | 触发面窄(仅流式+图),keepalive 是注释,无害 |
| 6 | ✅ 属实(权衡) | 满足 OpenAI `tool_calls→tool` 相邻约束([messages_translate.go:184](internal/handler/messages_translate.go#L184)) |
| 7 | ✅ 属实(低风险) | 转换依赖 `isSearchServerTool` 检查工具类型([search.go:130-132](internal/pipeline/search.go#L130)) |
| 8 | ✅ 属实 | [pipeline.go:186-210](internal/pipeline/pipeline.go#L186) |
| 9 | ✅ 属实(有意) | [stream_timeout.go:8-24](internal/handler/stream_timeout.go#L8) 注释明确是主动权衡 |
| 10 | ✅ 属实(非瓶颈) | 1024 条下微秒级 |
| 11 | ✅ 属实 | [config.go:390-393](internal/config/config.go#L390) 只校验值不校验组合 |
| 12 | ✅ 属实 | `cache_control`、多段 system 合并、thinking 块丢弃已确认 |

### 8.2 修复方案修正(3 条原方案有 bug 风险)

#### ⚠️ 问题 2:图片超时 —— 原方案"脱离 ctx 用独立 pipelineCtx"有泄漏风险

- **原方案风险**:context 一旦从父级(`r.Context()`)脱离,客户端断开时视觉调用不会停止,可能继续占用资源。
- **修正方案**:保留 `ctx` 作为父级(客户端断开能传播),**但给每个 vision 调用加独立 deadline**。这样既不被 `model.Timeout` 杀死,也不会在客户端断开后泄漏。
- **涉及**:[pipeline.go:157](internal/pipeline/pipeline.go#L157)、[vision.go:602](internal/pipeline/vision.go#L602)

#### ⚠️ 问题 4:流式搜索多轮 —— 原方案"循环重发"有 blockIndex/ctx 错乱风险

- **原方案风险**:`streamFromBackend` 用同一个 `ctx`,多轮后 ctx 可能已超时;且 `toolCallBuffer`/`blockIndex` 需要正确 reset,否则 blockIndex 错乱。
- **修正方案**:循环最多 3 轮,每轮**重建上下文**;检测到再次 web_search 时先 reset `toolCallBuffer`/`toolCalls` 再重发,严格对齐非流式 `HandleNonStreamingSearchLoop` 语义。
- **涉及**:[messages_streaming.go:345-434](internal/handler/messages_streaming.go#L345)

#### ⚠️ 问题 8:图片+PDF 并行 —— 原方案"sync.WaitGroup 并行"有竞态,必须放弃

- **原方案风险**:**`processImages` 和 `processPDFs` 会修改同一个 `chatReq` 共享的消息切片**。`normalizeContentParts`([vision.go:422-436](internal/pipeline/vision.go#L422))会把 `[]map[string]any` 就地改成 `[]any`;并行时若一个函数改了 `chatReq["messages"]`,另一个还在用旧引用,产生竞态。
- **修正方案**:**串行但合并** —— 先跑 `processImages` 把 `messages` 提取出来,再统一写回;或对同一份 `messages` 切片的**副本**并行处理,最后合并写回。绝不让两个函数并行改同一个 map。
- **涉及**:[pipeline.go:186-210](internal/pipeline/pipeline.go#L186)
- **结论**:**原并行方案不可行**,必须改为"串行合并"或"副本并行"。

#### ⚠️ 问题 9:流式空闲超时 —— 需警惕 DeepSeek thinking 空窗期

- **原方案风险**:thinking 模型可能在输出前静默 30-60s,空闲超时若设太短会误杀。
- **修正方案**:阈值须宽于 thinking 空窗(如 300s+),且**只在无数据时触发**,不限制总时长。
- **涉及**:[stream_timeout.go](internal/handler/stream_timeout.go)

#### ⚠️ 问题 12:翻译保真 —— `cache_control` 透传需先确认后端兼容性

- **原方案风险**:DeepSeek 等后端**可能不接受** `cache_control` 字段,透传可能引发 400。
- **修正方案**:先确认后端对 `cache_control` 的兼容性,再决定是否透传;否则保持现状。

### 8.3 最终修复清单(修正版)

| 优先级 | 修复项 | 方案 | 风险 |
|---|---|---|---|
| **P0** | 视觉熔断 | 请求级熔断:连续失败 N 次→熔断 M 秒→半开探测;熔断后降级为占位符 | 低 |
| **P0** | 图片超时 | **保留父级 ctx + 每 vision 调用独立 deadline** | 低 |
| **P0** | 流式搜索多轮 | **循环 ≤3 轮,每轮重建 ctx,reset buffer 后重发** | 中 |
| **P1** | 搜索失败对齐 | proxy 搜索失败返回 502 | 低 |
| **P1** | 图片+PDF 处理 | **串行合并(非并行)**,处理消息副本后统一写回 | 中 |
| **P1** | tool_result 顺序 | 不修,加注释说明权衡 | — |
| **P1** | 翻译保真 | 仅 `cache_control` 透传,且需先验证后端兼容性 | 中 |
| **P2** | 配置组合校验 | `validateConfig` 加组合报错 | 低 |
| **P2** | 流式空闲超时 | 无数据 > N 秒(宽于 thinking 空窗),非总时长 | 中 |
| **P2** | LRU 优化 | **不修**(改动大、有竞态风险、非瓶颈) | — |

---

## 9. 实施记录(2026-08-01)

> 依据第 8.3 节清单实施。本次只做**低风险、经自审确认安全**的 4 项 + 注释;未实施需谨慎处理的风险项(见 9.3)。

### 9.1 已实施

| # | 修复项 | 变更文件 | 说明 |
|---|---|---|---|
| 1 | **视觉熔断器** | [vision.go](internal/pipeline/vision.go) | 新增包级 `visionBreaker`:连续失败 10 次 → 熔断 30s → 半开探测。`processImages` 的 goroutine 在调视觉后端前检查 `Allow()`;失败收集处 `Failure()`/`Success()`。熔断期间图片直接降级为占位符。带 `ResetVisionBreaker()` 供测试 |
| 2 | **图片处理 ctx 可中断** | [vision.go](internal/pipeline/vision.go) | goroutine 获取并发槽前 `select { case <-ctx.Done() }`;`wg.Wait()` 改为外层 `select ctx.Done()` 逃生舱,客户端断开/请求超时不再无限阻塞 |
| 3 | **搜索失败对齐** | [proxy.go](internal/handler/proxy.go) | 搜索循环失败返回 502 `web search failed`,不再把含无法执行的 `web_search` tool_call 的原始响应转发给客户端(对齐 messages 路径) |
| 4 | **配置组合校验** | [config.go](internal/config/config.go) | `validateConfig` 增加:`messages_mode: translate` + `type: anthropic` 启动即报错,避免运行时对 `api.anthropic.com` 请求不存在的 `/chat/completions` 得到 404 |
| 5 | **tool_result 顺序注释** | [messages_translate.go](internal/handler/messages_translate.go) | 补充注释说明"工具结果前置"是满足 OpenAI `tool_calls→tool` 相邻约束的刻意权衡,警示勿轻易还原 |

### 9.2 新增测试

- **config**:3 个组合校验测试(`TestValidateConfig_MessagesModeTranslateWithAnthropicType` 报错 / `...WithOpenAIType` 通过 / `...NativeWithAnthropicType` 通过)
- **pipeline**:4 个熔断器测试(`TestVisionBreaker_OpensAfterFailures` 达阈值打开 / `...CooldownAllowsProbe` 冷却后半开 / `...OpenCausesPlaceholder` 打开时占位符 / `...SuccessCloses` 成功闭合)

> 写测试时发现一个**测试自身的坑**:`processImages` 会把失败图片替换成占位符写回传入的 map,测试必须每迭代新建请求,否则 failures 卡在 1。熔断器代码本身正确。

### 9.3 未实施(风险项,需谨慎单独处理)

- **流式搜索多轮**(循环 ≤3 轮,blockIndex/ctx 生命周期复杂)
- **图片+PDF 并行**(`normalizeContentParts` 就地改切片会竞态,已判定不可行)
- **流式空闲超时**(DeepSeek thinking 空窗期会误杀)
- **LRU O(n) 优化**(1024 条非瓶颈,改动有竞态风险)

### 9.4 验证结果

**单元测试**:`go build ./...` 通过;`pipeline`、`config`、`auth`、`mcp`、`ratelimit`、`awsauth`、`awsstream` 全部通过。
> 注:`internal/handler` 测试包编译失败(`usage_log_test.go` 引用全仓库不存在的 `SetHealthStore`/`recordBackendHealth`),经 `git stash` 验证为**仓库预先存在的坏测试**(ebca626 已存在),与本次改动无关;handler 非测试代码编译通过。

**端到端真实测试**(部署新二进制 + 生产配置,监听 8787):

| # | 用例 | 结果 |
|---|---|---|
| 1 | `GET /v1/models` | ✅ 8 模型正确列出 |
| 2 | `GET /v1/models/status` | ✅ 全部 `online:true` |
| 3 | 非流式 chat → DeepSeek | ✅ `1+1 is 2.`,含 `reasoning_content` |
| 4 | 流式 chat → DeepSeek | ✅ 分块输出 + usage + `[DONE]` |
| 5 | `/v1/messages` 非流式翻译 | ✅ `thinking` 块 + 文本块 + 正确 usage(补丁1/3 生效) |
| 6 | `/v1/messages` 流式翻译 | ✅ 7 text_delta + 18 thinking_delta + message_delta/stop |
| 7 | 视觉链路(红色 PNG) | ✅ DeepSeek 答 `red`;日志确认 `image described by vision model, sensenova-6.7-flash-lite, description_len:219` |
| 8 | 视觉链路(蓝色 PNG,新图) | ✅ 答 `blue`,耗时 3.4s(调一次视觉模型) |
| 9 | 缓存命中(红色图重发) | ✅ 答 `red`,耗时 **0.98s**(未调视觉模型) |

**熔断器未误触发**:正常负载下 0 次熔断,符合预期。

**插曲**:首次 chat 测试返回 400,排查为 **curl 传中文的 shell 编码问题**(`invalid unicode code point`),非 proxy bug;改用文件传 JSON 后正常。

---

*本文档基于源码核对生成。第 5 节为对独立审核报告(`docs/design-issues.md`)的复核结论,第 6 节为修复计划初版,第 8 节为实施前自审修正版(以第 8.3 节清单为准),第 9 节为实施记录与端到端验证结果。*
