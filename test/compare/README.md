# PDF 处理结果比对：go-llm-proxy vs MinerU

> **⚠️ 已过时（2026-08）：** 本页记录的是 **MinerU 集成之前** go-llm-proxy 旧 PDF 管线的比对结论。
> go-llm-proxy 的 `processPDFs` 现已**完全替换为 MinerU 云 API 管线**（见 `docs/pipeline.md`）——
> 旧的三级级联（ledongthuc 文本提取 → OCR → vision）已删除。下方"go-llm-proxy 输出 ~5.9K + 236 乱码"
> 等结论仅对当时的旧管线成立。此文档保留作为历史比对记录，不再反映当前行为。

对同一份 PDF（`sample.pdf`），分别用 **go-llm-proxy 的 PDF 管线** 和 **MinerU 云 API** 处理，
逐维度比对两者喂给 LLM 的内容差异。

## 产物

- **比对报告**：`out/compare/report.md`（由 `compare.py` 自动生成）
- **go-llm-proxy 输出**：`out/compare/go_llm_proxy_output_{openai,anthropic}.txt`
- **MinerU 输出**：`out/compare/mineru/{pipeline,vlm}/`（full.md + content_list + images/）
- **样例元数据**：`out/compare/pdfinfo.json`（pdfinfo/pdfimages 快照）

## 一键执行

```bash
# proxy 采集 → MinerU 云采集 → 比对报告
bash test/compare/scripts/run_all.sh
```

需要：
- Python 3 + `requests`（`pip install requests`）
- MinerU 云 API token：写入仓库根 `.env` 的 `MINERU_APIKEY=`（已 gitignore），
  或环境变量 `MINERU_API_TOKEN`

## 分步执行

```bash
# 1) 只采集 go-llm-proxy 侧（离线，起临时 proxy 实例 + 本地 mock）
bash test/compare/scripts/collect_proxy.sh all   # openai + anthropic 两种协议

# 2) 只采集 MinerU 云 API（pipeline + vlm）
python test/compare/scripts/collect_mineru.py --pdf sample.pdf \
  --out out/compare/mineru --model pipeline --model vlm

# 3) 生成报告
python test/compare/scripts/compare.py --out out/compare --pdfinfo out/compare/pdfinfo.json
```

## 原理

go-llm-proxy 处理 PDF 后，产物（`<pdf_content>` 文本块）只存在于它转发给上游 LLM 的
请求里。因此用一个**本地 mock OpenAI server**（`mock_openai.py`）作为 proxy 测试实例的
backend，记录转发的请求体，即可拿到 proxy 实际注入 LLM 的内容。

`collect_proxy.sh` 用独立端口（8788）起一个隔离的 proxy 实例，不影响生产实例（8787）。

## 请求协议

| 协议 | 端点 | PDF 载体 | 对应客户端 |
|---|---|---|---|
| openai | `POST /v1/chat/completions` | `image_url` + `data:application/pdf` | ChatGPT/OpenCode |
| anthropic | `POST /v1/messages` | `document` block | **Claude Code**（生产主路径） |

两种协议都归一化到 proxy 内部的 `pdf_data` 块，走同一套 `processPDFs` 三级管线
（文本提取 → OCR → vision）。

## 已知结论（sample.pdf = PowerPoint 转 PDF，12 页，98 张内嵌图）

- **go-llm-proxy**：走 Stage 1 纯文本提取（`ledongthuc/pdf`），输出 `~5.9K` 字符纯文本。
  图片全部丢弃；页眉页脚混入正文；**产生 236 个 U+FFFD 乱码**（Wingdings 等符号字形
  提取失败），乱码原样进入 LLM 上下文。
- **MinerU**：pipeline/vlm 均输出结构化 markdown + content_list（title/paragraph/image/
  chart/equation_interline 块）+ 28 张裁剪图 + 9 个 LaTeX 公式。页眉页脚基本剥离。
  文本保真度（与 proxy 的 bigram 相似度）约 67%。
- 详见 `out/compare/report.md`。

## 图片转文字方案（MinerU 结构化 + vision 描述 → 纯文本喂 LLM）

MinerU 的 full.md 里图片只是 `![](images/xxx.jpg)` 引用，纯文本 LLM 看不到。
方案把图片转成文字描述，让 PDF 内容对纯文本 LLM 完全可用：

```
PDF → MinerU 云 API → 结构化产物 + images/
  → images_to_text.py（调 vision 模型，把裁剪图转成描述）
  → merge_to_pure_text.py（把 full.md 的图片引用替换为描述）
  → pure_text.md（纯文本，无图片引用）
```

```bash
# 阶段1：图片 → 文字描述（默认 qwen-vl-max，可用 config.yaml 其他 vision 模型）
python test/compare/scripts/images_to_text.py --mineru-dir out/compare/mineru/vlm

# 阶段2：合并 full.md + 描述 → pure_text.md
python test/compare/scripts/merge_to_pure_text.py --mineru-dir out/compare/mineru/vlm

# 阶段3（可选）：同一组理解题分别喂 baseline 和 candidate，对比回答质量
python test/compare/scripts/comprehension_check.py
```

**实测结论**（`out/compare/comprehension.md`）：把图片描述注入后，LLM 能回答图像内容
问题（如 Fig6 是 BER 盒图对比、MC-VQVAE vs EXNN），这是纯文本提取无法做到的。
注意：DeepSeek 等推理模型需足够 `max_tokens`，否则 token 耗尽于思考链、content 为空。
