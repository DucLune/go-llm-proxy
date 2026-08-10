#!/usr/bin/env python3
"""Compare go-llm-proxy's PDF output vs MinerU cloud API output for sample.pdf.

Inputs (created by collect_proxy.sh and collect_mineru.py):
  out/compare/go_llm_proxy_output_{openai,anthropic}.txt
  out/compare/mineru/{pipeline,vlm}/  (full.md, *_content_list.json, images/, ...)

Outputs a Markdown report at out/compare/report.md.

Pure stdlib.
"""
import argparse
import json
import re
import sys
from pathlib import Path

# Regex to strip the proxy's XML wrapper and isolate the actual injected text.
PDF_CONTENT_RE = re.compile(r"<pdf_content\b[^>]*>(.*?)</pdf_content>", re.S)


def load_proxy_output(path):
    """Return the injected PDF text (inner content of <pdf_content>)."""
    text = Path(path).read_text(encoding="utf-8")
    m = PDF_CONTENT_RE.search(text)
    if m:
        return m.group(1).strip()
    return text.strip()


def strip_pdf_headers_footers(text):
    """Rough strip of the repeated 'PIERS 2026 / 2026/7/29NN' header/footer lines."""
    lines = []
    for ln in text.splitlines():
        s = ln.strip()
        if re.fullmatch(r"PIERS 2026", s):
            continue
        if re.fullmatch(r"2026/7/29\d*", s):
            continue
        lines.append(s)
    return "\n".join(lines)


def page_split(text):
    """Split proxy text into pseudo-pages at the header/footer boundary markers."""
    # proxy output has no explicit page markers; split on the repeated footer
    # that ends each slide ('PIERS 2026\n2026/7/29NN')
    chunks = re.split(r"(?=\bPIERS 2026\b)", text)
    return [c.strip() for c in chunks if c.strip()]


def char_diff_stats(a, b):
    """Very rough normalized-character overlap between two text blobs."""
    # Keep only alphanumerics (incl. CJK) for comparison
    def norm(s):
        return re.sub(r"[\W_]+", "", s.lower())

    na, nb = norm(a), norm(b)
    if not na or not nb:
        return {"similarity": 0.0, "len_a": len(a), "len_b": len(b)}
    # count shared bigrams
    big_a = {na[i:i + 2] for i in range(len(na) - 1)}
    big_b = {nb[i:i + 2] for i in range(len(nb) - 1)}
    inter = big_a & big_b
    union = big_a | big_b
    return {
        "similarity": len(inter) / len(union) if union else 0.0,
        "len_a": len(a),
        "len_b": len(b),
    }


def mineru_content_list(mineru_dir):
    """Find *_content_list.json (or v2) under a MinerU result dir; return blocks."""
    p = Path(mineru_dir)
    if not p.exists():
        return None
    # Prefer v2 (flattened, per-page groups) since it carries caption spans;
    # fall back to v1.
    files = sorted(p.glob("*_content_list_v2.json")) or sorted(p.glob("*_content_list.json"))
    if not files:
        return None
    try:
        data = json.loads(files[0].read_text(encoding="utf-8"))
    except Exception:
        return None
    if isinstance(data, list) and data and isinstance(data[0], list):
        return [b for page in data for b in page]
    return data


def mineru_stats(mineru_dir):
    """Aggregate a summary of MinerU's structured output."""
    blocks = mineru_content_list(mineru_dir)
    images = sorted((Path(mineru_dir) / "images").glob("*")) if (Path(mineru_dir) / "images").exists() else []
    stat = {
        "dir": str(mineru_dir),
        "image_files": len(images),
        "image_names": [i.name for i in images][:8],
        "blocks": len(blocks) if blocks else 0,
    }
    if blocks:
        types = {}
        for b in blocks:
            t = b.get("type", "?")
            types[t] = types.get(t, 0) + 1
        stat["block_types"] = types
        img_blocks = [b for b in blocks if b.get("type") == "image"]
        chart_blocks = [b for b in blocks if b.get("type") == "chart"]
        stat["image_blocks"] = len(img_blocks)
        stat["chart_blocks"] = len(chart_blocks)

        def block_caption(b, key):
            """Caption from either v1 (top-level list) or v2 (content dict spans)."""
            direct = b.get(key)
            if isinstance(direct, list) and direct:
                return True
            c = b.get("content")
            if isinstance(c, dict):
                cap = c.get(key)
                if isinstance(cap, list) and cap:
                    return True
                if isinstance(cap, str) and cap.strip():
                    return True
            return False

        stat["image_captions"] = sum(
            1 for b in img_blocks if block_caption(b, "image_caption")
        )
        stat["chart_captions"] = sum(
            1 for b in chart_blocks if block_caption(b, "chart_caption")
        )

        def block_description(b):
            """Semantic description: v1 string content, or v2 content['content']."""
            c = b.get("content")
            if isinstance(c, str) and c.strip():
                return c.strip()
            if isinstance(c, dict):
                inner = c.get("content")
                if isinstance(inner, str) and inner.strip():
                    return inner.strip()
            return ""

        stat["image_blocks_with_description"] = sum(
            1 for b in img_blocks if block_description(b)
        )
        stat["chart_blocks_with_description"] = sum(
            1 for b in chart_blocks if block_description(b)
        )
        # equations: v1 type="equation", v2 type="equation_interline"
        eq = [
            b for b in blocks
            if b.get("type") in ("equation", "equation_interline")
        ]
        stat["equation_blocks"] = len(eq)
        stat["equation_samples"] = []
        for b in eq[:3]:
            t = b.get("text") or (b.get("content", {}).get("math_content") if isinstance(b.get("content"), dict) else None)
            if isinstance(t, list):
                t = " ".join(s.get("content", "") for s in t if isinstance(s, dict))
            stat["equation_samples"].append(str(t or "")[:100])
        # markdown for structural check
    md_files = sorted((Path(mineru_dir)).glob("full.md"))
    if md_files:
        stat["markdown_len"] = len(md_files[0].read_text(encoding="utf-8"))
    return stat


def count_images_in_pdf_proxy(text):
    """Heuristic: proxy output mentions images? It doesn't extract them."""
    return 0


def build_report(proxy_files, mineru_dirs, pdfinfo):
    # pdfinfo keys: "Pages" (pdfinfo) — normalize to lower for stable access.
    norm = {k.lower(): v for k, v in pdfinfo.items()}
    lines = []
    lines.append("# PDF 处理结果比对：go-llm-proxy vs MinerU")
    lines.append("")
    pages = norm.get("pages", norm.get("pages", "?"))
    lines.append(
        f"> 样例: `{norm.get('file', 'sample.pdf')}`  {pages} 页 · "
        f"{norm.get('producer', '')}"
    )
    lines.append("")
    lines.append("## 1. 概览")
    lines.append("")
    lines.append("| 维度 | go-llm-proxy | MinerU pipeline | MinerU vlm |")
    lines.append("|---|---|---|---|")

    # load proxy outputs
    proxy = {}
    for name, path in proxy_files.items():
        proxy[name] = load_proxy_output(path)

    # MinerU stats
    mstats = {mv: mineru_stats(d) for mv, d in mineru_dirs.items()}

    # --- dimension 1: text fidelity ---
    lines.append("")
    lines.append("## 2. 文本保真度")
    lines.append("")
    for name, text in proxy.items():
        stats = char_diff_stats(text, text)  # baseline no-op
    # Compare proxy text against MinerU markdown text (both should contain the
    # slide text). Use similarity of bigrams.
    mineru_md_text = {}
    for mv, d in mineru_dirs.items():
        p = Path(d)
        md = p / "full.md"
        mineru_md_text[mv] = md.read_text(encoding="utf-8") if md.exists() else ""
    for name, text in proxy.items():
        parts = []
        for mv, mdt in mineru_md_text.items():
            if mdt:
                parts.append(f"{mv}={char_diff_stats(text, mdt)['similarity']:.2%}")
            else:
                parts.append(f"{mv}=待采集")
        lines.append(f"- **proxy[{name}]** vs MinerU markdown 相似度: {', '.join(parts)}")
    lines.append("")

    # --- dimension 3: images ---
    lines.append("## 3. 图片处理")
    lines.append("")
    lines.append(f"- PDF 内嵌图片数(pdfimages): `{pdfinfo.get('images', '?')}`")
    for name, text in proxy.items():
        lines.append(f"- **proxy[{name}]**: 提取文本 {len(text)} 字符；图片相关输出: **0**（纯文本提取，图片被丢弃）")
    for mv, st in mstats.items():
        if st["blocks"]:
            lines.append(
                f"- **MinerU[{mv}]**: 图片块 {st.get('image_blocks', 0)} 个 / 图表块 {st.get('chart_blocks', 0)} 个；"
                f"带 VLM 语义描述: 图 {st.get('image_blocks_with_description', 0)} / 表 {st.get('chart_blocks_with_description', 0)}；"
                f"裁剪图 {st['image_files']} 张；图注 {st.get('image_captions', 0)} 条，表注 {st.get('chart_captions', 0)} 条"
            )
    lines.append("")

    # --- dimension 4: equations ---
    lines.append("## 4. 公式处理")
    lines.append("")
    for name, text in proxy.items():
        lines.append(f"- **proxy[{name}]**: 公式被线性化为普通文本（示例见正文），LaTeX 结构丢失")
    for mv, st in mstats.items():
        if st["blocks"]:
            lines.append(
                f"- **MinerU[{mv}]**: 公式块 {st.get('equation_blocks', 0)} 个；样例: "
                + "; ".join(st.get("equation_samples", []) or ["(无)"])
            )
    lines.append("")

    # --- dimension 5: reading order / structure ---
    lines.append("## 5. 阅读顺序与结构")
    lines.append("")
    for name, text in proxy.items():
        lines.append(f"- **proxy[{name}]**: 纯线性文本流，无标题层级/块结构；页眉页脚页码混入正文")
    for mv, st in mstats.items():
        if st["blocks"]:
            types = ", ".join(f"{k}={v}" for k, v in st["block_types"].items())
            lines.append(f"- **MinerU[{mv}]**: 结构化块类型分布 → {types}")
    lines.append("")

    # --- dimension 6: header/footer handling ---
    lines.append("## 6. 页眉页脚处理")
    lines.append("")
    for name, text in proxy.items():
        has_hf = bool(re.search(r"PIERS 2026|2026/7/29\d", text))
        lines.append(f"- **proxy[{name}]**: 页眉页脚{'混入正文' if has_hf else '未检出'}（'PIERS 2026' / '2026/7/29NN' 每页重复）")
    for mv, st in mstats.items():
        lines.append(f"- **MinerU[{mv}]**: full.md 中页眉页脚{'已剥离' if not (st.get('markdown_len', 0) and 'PIERS 2026' in (Path(st['dir'])/'full.md').read_text(encoding='utf-8', errors='ignore')) else '仍存在'}")
    lines.append("")

    # --- dimension 7: filename metadata ---
    lines.append("## 7. 元数据保留")
    lines.append("")
    lines.append("- **proxy[openai]**: 请求中无 filename，输出 `<pdf_content source=\"text\">`（无文件名）")
    lines.append("- **proxy[anthropic]**: document block 携带 title，输出 `<pdf_content filename=\"sample.pdf\" source=\"text\">`")
    lines.append("")
    lines.append("---")
    lines.append("")
    lines.append("*报告由 `compare.py` 自动生成。*")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="out/compare")
    ap.add_argument("--pdfinfo", default="")
    args = ap.parse_args()

    out = Path(args.out)
    proxy_files = {
        "openai": out / "go_llm_proxy_output_openai.txt",
        "anthropic": out / "go_llm_proxy_output_anthropic.txt",
    }
    mineru_dirs = {
        "pipeline": out / "mineru" / "pipeline",
        "vlm": out / "mineru" / "vlm",
    }
    pdfinfo = {}
    if args.pdfinfo and Path(args.pdfinfo).exists():
        import csv
        with open(args.pdfinfo, encoding="utf-8") as fh:
            pdfinfo = json.load(fh)

    report = build_report(proxy_files, mineru_dirs, pdfinfo)
    report_path = out / "report.md"
    report_path.write_text(report, encoding="utf-8")
    print(f"report written -> {report_path}")


if __name__ == "__main__":
    main()
