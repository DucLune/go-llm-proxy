#!/usr/bin/env python3
"""Phase 3 (optional): end-to-end comprehension check via a text-only LLM.

Feeds the SAME comprehension questions to a text-only LLM twice:
  - baseline: go-llm-proxy's Stage-1 extraction (go_llm_proxy_output_*.txt)
  - candidate: MinerU + image descriptions (pure_text.md)

The questions target information that lives IN the images/figures (which the
baseline drops). This makes the quality gap measurable rather than anecdotal.

Uses the production go-llm-proxy instance (127.0.0.1:8787) with the DeepSeek
model, or an arbitrary OpenAI-compatible endpoint via flags.

Pure stdlib. Requests go to the proxy, which forwards to the configured model.
"""
import argparse
import json
import sys
from pathlib import Path

try:
    import requests
except ImportError:
    sys.stderr.write("ERROR: need `requests`. Run: pip install requests\n")
    sys.exit(1)

QUESTIONS = [
    "这篇论文提出的方法叫什么？核心创新点是什么？",
    "论文的实验中用了什么类型的LED和光电探测器？(关键词：Hamamatsu、PIN)",
    "Fig6展示了什么结果？不同均衡方法的BER表现如何？",
    "论文提到的三种均衡方法(VQVAE、CMA-DD、MC-VQVAE)在稳定性上有什么区别？",
    "MC-VQVAE 的核心损失函数由哪几部分组成？各自的作用是什么？",
]


def chat(api, model, key, system, question, timeout=120):
    payload = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": question},
        ],
        # DeepSeek reasoning models spend tokens in the thinking channel; give
        # enough headroom or content can come back empty with finish=length.
        "max_tokens": 2000,
    }
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    resp = requests.post(api.rstrip("/") + "/v1/chat/completions",
                         headers=headers, json=payload, timeout=timeout)
    resp.raise_for_status()
    data = resp.json()
    choices = data.get("choices") or []
    if not choices:
        return "(empty response)"
    msg = choices[0].get("message") or {}
    content = msg.get("content", "")
    # Reasoning-model fallback: if content is empty but reasoning_content has
    # text (DeepSeek put the answer in the thinking channel), surface it.
    if not content.strip() and (msg.get("reasoning_content") or "").strip():
        content = "[reasoning] " + msg["reasoning_content"].strip()
    return content or "(empty)"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--api", default="http://127.0.0.1:8787",
                    help="OpenAI-compatible endpoint (default: go-llm-proxy)")
    ap.add_argument("--model", default="deepseek-v4-flash",
                    help="model to ask (default: deepseek-v4-flash)")
    ap.add_argument("--key", default="sk-local-dev-9f3a2c",
                    help="Bearer key (default: local proxy key)")
    ap.add_argument("--baseline", default="out/compare/go_llm_proxy_output_anthropic.txt")
    ap.add_argument("--candidate", default="out/compare/mineru/vlm/pure_text.md")
    ap.add_argument("--out", default="out/compare/comprehension.md")
    args = ap.parse_args()

    baseline = Path(args.baseline).read_text(encoding="utf-8")
    candidate = Path(args.candidate).read_text(encoding="utf-8")

    lines = ["# LLM 理解质量对比（纯文本 LLM 问答）",
             "",
             f"- baseline: {args.baseline}（{len(baseline)} chars）",
             f"- candidate: {args.candidate}（{len(candidate)} chars）",
             f"- 提问模型: {args.model}",
             ""]
    for i, q in enumerate(QUESTIONS, 1):
        lines.append(f"## Q{i}. {q}")
        lines.append("")
        for label, text in (("BASELINE", baseline), ("CANDIDATE", candidate)):
            sys_ctx = (
                "你是一个文档理解助手。以下是用户提供的一份文档内容，请基于它回答用户问题。"
                "如果文档中确实没有相关信息，请如实说明'文档中未提及'。\n\n文档内容：\n" + text
            )
            try:
                ans = chat(args.api, args.model, args.key, sys_ctx, q)
            except Exception as e:
                ans = f"(调用失败: {e})"
            lines.append(f"### {label}")
            lines.append("")
            lines.append(ans)
            lines.append("")
            print(f"Q{i} {label} done ({len(ans)} chars)", flush=True)

    out_path = Path(args.out)
    out_path.write_text("\n".join(lines), encoding="utf-8")
    print(f"comparison -> {out_path}")


if __name__ == "__main__":
    main()
