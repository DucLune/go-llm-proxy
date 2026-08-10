#!/usr/bin/env python3
"""Phase 2: Merge MinerU's structured markdown with image descriptions into pure text.

Reads a MinerU result dir's full.md plus the images_descriptions.jsonl produced
by images_to_text.py, and replaces every `![](images/xxx.jpg)` image reference
with a text block of the form:

    [图 N: {caption}]
    {description}

The result (pure_text.md) contains no image references at all, so a text-only
LLM can read it fully. Verifies no `![](images/` references survive.

Pure stdlib.
"""
import argparse
import json
import re
import sys
from pathlib import Path

IMG_REF_RE = re.compile(r"!\[([^\]]*)\]\((images/[^)\s]+)\)")


def load_descriptions(mineru_dir):
    """Load images_descriptions.jsonl -> {img_path: rec}."""
    p = Path(mineru_dir) / "images_descriptions.jsonl"
    out = {}
    if not p.exists():
        return out
    for line in p.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        rec = json.loads(line)
        out[rec["img_path"]] = rec
    return out


def merge(mineru_dir):
    md_files = sorted(Path(mineru_dir).glob("full.md"))
    if not md_files:
        sys.stderr.write(f"no full.md under {mineru_dir}\n")
        return None
    md = md_files[0].read_text(encoding="utf-8")

    descs = load_descriptions(mineru_dir)
    missing = []

    def repl(m):
        alt, path = m.group(1), m.group(2)
        rec = descs.get(path)
        if not rec:
            missing.append(path)
            return f"[图: 无描述, 图片缺失 {path}]"
        caption = rec.get("caption") or alt or ""
        description = (rec.get("description") or "").strip()
        if not description:
            missing.append(path)
            return f"[图: {caption}] (无图片描述)"
        head = f"[图: {caption}]" if caption else "[图]"
        return f"{head}\n{description}"

    merged = IMG_REF_RE.sub(repl, md)
    # Report any image references that were NOT matched (format mismatch).
    leftover = IMG_REF_RE.findall(merged)
    return merged, missing, leftover


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--mineru-dir", default="out/compare/mineru/vlm",
                    help="MinerU result dir (with full.md + images_descriptions.jsonl)")
    ap.add_argument("--out", default=None,
                    help="output path (default: <mineru-dir>/pure_text.md)")
    args = ap.parse_args()

    result = merge(args.mineru_dir)
    if result is None:
        sys.exit(1)
    merged, missing, leftover = result

    out_path = args.out or (Path(args.mineru_dir) / "pure_text.md")
    out_path.write_text(merged, encoding="utf-8")

    # --- verification ---
    img_refs = IMG_REF_RE.findall(merged)
    print(f"output: {out_path}")
    print(f"markdown length: {len(merged)} chars")
    print(f"residual `![](images/` references: {len(img_refs)}")
    print(f"image blocks without description: {len(missing)}")
    if img_refs:
        print("  WARNING leftover:", img_refs[:5])
    if missing:
        print("  WARNING missing desc:", missing[:5])
    if not img_refs and not missing:
        print("OK: pure_text.md has no image references and all images described.")


if __name__ == "__main__":
    main()
