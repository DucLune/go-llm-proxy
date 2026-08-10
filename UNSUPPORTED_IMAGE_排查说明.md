# `[Unsupported Image]` 排查说明

> 调查时间：2026-08-01
> 结论一句话：**`[Unsupported Image]` 是 Claude Code 客户端（VSCode 扩展/CLI）在图片无法被模型接收时自行渲染的占位符，与 CC switch、go-llm-proxy、DeepSeek 上游均无关。**

---

## 一、背景

用户使用无原生识图能力的 DeepSeek 模型 + claude-vision-skill 时，在 Claude Code 对话框里发图片，消息显示 `[Unsupported Image]`。需要确认这个标记到底是谁产生的。

当时链路可能经过多个中间层：
- **CC switch**（本地代理，监听 15721，含"整流器"功能）
- **go-llm-proxy**（本地代理，监听 8787，含 vision 图片转文字管线）
- **Claude Code 客户端**（VSCode 扩展 + CLI）
- **DeepSeek 上游 API**

---

## 二、排查过程

### 2.1 排查 CC switch 源码

源码仓库：`farion1231/cc-switch`（经本地代理 clone）

- **确认存在"整流器"**：`src-tauri/src/proxy/media_sanitizer.rs`
  ```rust
  pub const UNSUPPORTED_IMAGE_MARKER: &str = "[Unsupported Image]";
  ```
- 该标记是 **CC switch 全仓库唯一定义**处，功能是把图片块替换为 `[Unsupported Image]`
- 但替换逻辑受 `RectifierConfig.request_media_fallback` 门控（`src-tauri/src/proxy/forwarder.rs`）：
  ```rust
  if !(self.rectifier_config.enabled && self.rectifier_config.request_media_fallback) {
      return 0;
  }
  ```
- **用户本地数据库**（`~/.cc-switch/cc-switch.db` → `settings` 表 `rectifier_config`）：
  ```json
  {"enabled":true,"requestMediaFallback":false,"requestMediaHeuristic":true,...}
  ```
  → `requestMediaFallback` 已关闭。
- **运行日志**（`~/.cc-switch/logs/cc-switch.log`）：8/1 全天 `[Media] Replaced` 记录为 **0 次**。

**结论：8/1 当天 CC switch 整流器未触发，不产生该标记。**

> 注：7/31 日志中确有 104 次替换记录（当时整流器还开着），那些是历史对话里留下的 `[Unsupported Image]` 文本。

### 2.2 排查 go-llm-proxy

- 源码（`d:/Git/go-llm-proxy`，Go）：grep 全部 `.go` 文件，**无 `Unsupported Image` 定义**。
- 日志里虽有 232 处该字符串，但全是 `msg: "raw anthropic message"`、`role: "assistant"` 的 DEBUG 转储——即**记录的是 assistant（Claude）回复里打的文字**，不是它生成的标记。
- 它另有 vision 管线（`processors.vision: sensenova-6.7-flash-lite`），负责把图片转成 `<image_description>` 文本注入——**这是另一套机制**，与 `[Unsupported Image]` 无关。

**结论：go-llm-proxy 不产生该标记。**

### 2.3 排查 Claude Code 客户端

搜索范围：
- VSCode 扩展 `anthropic.claude-code-2.1.220-win32-x64`（`extension.js`、`webview/index.js`）
- npm 全局包 `@anthropic-ai/claude-code@2.1.220`（`bin/claude.exe`，254MB，跨 utf8/latin1/utf16le 编码）
- `~/.claude` 目录（273 个 JS/JSON/TS 文件）

**直接字符串搜索均未命中** `[Unsupported Image]`（疑似压缩/混淆，但源码层无明文）。当时保留"客户端嫌疑"但证据不足。

### 2.4 实验 1：直连 DeepSeek Anthropic API（绕过一切代理）

用 Node 直接向 `api.deepseek.com/anthropic/v1/messages` 发送 **1×1 红色 PNG（base64）+ 文字**：

```json
{
  "model": "deepseek-v4-flash",
  "messages": [{
    "role": "user",
    "content": [
      { "type": "image", "source": { "type": "base64", "media_type": "image/png", "data": "..." } },
      { "type": "text", "text": "请回复: 这张图是什么颜色? 如果看不到图, 回复看不到图" }
    ]
  }]
}
```

**结果**：HTTP 200，回复 `"text": "看不到图"`（thinking 中提及收到图片块但无法解析）。

**结论：DeepSeek 上游的回复不含 `[Unsupported Image]`。**

### 2.5 实验 2：改 settings.json 直连 DeepSeek（关掉 CC switch）

- 用户手动把 `~/.claude/settings.json` 改为直连：
  ```json
  {
    "env": {
      "ANTHROPIC_AUTH_TOKEN": "sk-64721241...",
      "ANTHROPIC_BASE_URL": "https://api.deepseek.com/anthropic",
      "ANTHROPIC_MODEL": "deepseek-v4-flash"
    }
  }
  ```
- 确认 **CC switch 已退出**（日志 19:32:44"清理完成，退出应用"；端口 15721 无监听）
- 当前配置不路由到 go-llm-proxy（8787）

**结果：在纯 Claude Code → DeepSeek 直连、无任何中间代理的情况下，发图片依然显示 `[Unsupported Image]`。**

---

## 三、最终结论

| 来源 | 是否产生 `[Unsupported Image]` | 依据 |
|------|:---:|------|
| **Claude Code 客户端**（VSCode 扩展/CLI） | ✅ **是** | 直连环境下仍出现 |
| DeepSeek 上游 API | ❌ | 实验 1：回复不含该标记 |
| CC switch 整流器 | ❌（8/1 起） | 开关已关，0 次触发 |
| go-llm-proxy | ❌ | 源码无此定义 |

**`[Unsupported Image]` 是 Claude Code 客户端在"图片无法传给模型（模型不支持视觉/格式不被接受）"时自行渲染的占位符。** 它不依赖任何代理，只要底层模型不支持图片就会出现。

---

## 四、两个易混淆的机制

| 标记 | 谁产生 | 触发条件 |
|------|--------|----------|
| `[Unsupported Image]` | **Claude Code 客户端** | 底层模型不支持图片输入时，客户端把图片块渲染成占位符 |
| `<image_description>` | **go-llm-proxy**（vision 管线） | 图片经 go-llm-proxy 转发时，被截取转成文字描述注入消息 |

- 走 **go-llm-proxy** 链路时：图片被转成 `<image_description>`，客户端侧因图片已被替换而显示 `[Unsupported Image]` —— 两者同时出现。
- 走**直连 DeepSeek** 链路时：只有 `[Unsupported Image]`（客户端生成），没有 `<image_description>`。

---

## 五、参考文件

| 文件 | 说明 |
|------|------|
| `~/.claude/settings.json` | Claude Code 配置（当前为直连 DeepSeek） |
| `~/.claude/settings.json.bak-before-gllm` | 之前直连 DeepSeek 的备份 |
| `~/.cc-switch/cc-switch.db` | CC switch 配置数据库（`settings.rectifier_config`） |
| `~/.cc-switch/logs/cc-switch.log` | CC switch 运行日志（含整流器替换记录） |
| `d:/Git/go-llm-proxy/config.yaml` | go-llm-proxy 配置（vision 管线、API key） |
| `d:/Git/go-llm-proxy/proxy.log` | go-llm-proxy 运行日志 |
| CC switch 源码 | `src-tauri/src/proxy/media_sanitizer.rs`、`forwarder.rs`、`types.rs` |

---

## 六、遗留事项

- Claude Code 客户端源码中未搜到 `[Unsupported Image]` 明文（可能压缩/混淆在 `claude.exe` 内），结论基于**行为实验**（直连环境复现），而非源码命中。
- 若需让无视觉模型正常"看图"，可继续使用 claude-vision-skill：由 `vision.js` 显式调用带视觉的模型（如千问）识别图片，并把结果作为文本注入。
