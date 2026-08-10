#!/usr/bin/env python3
"""Phase 1: Convert MinerU's cropped images to text descriptions via a vision model.

Reads a MinerU result dir (from collect_mineru.py) and, for every image/chart
block in *_content_list_v2.json, sends the cropped image to a vision model
(OpenAI-compatible chat completions) and records the description.

This mirrors go-llm-proxy's describeImage (internal/pipeline/vision.go): a
user-role image_url request with a describe prompt. It runs standalone so we can
validate image→text quality before deciding whether to wire it into the proxy.

Dependencies: requests. Vision backend/key come from config.yaml (default
qwen-vl-max) or --backend/--api-key/--model overrides.

Output: <mineru_dir>/images_descriptions.jsonl  (one JSON object per image block)
"""
import argparse
import base64
import json
import sys
from pathlib import Path

try:
    import requests
    import yaml
except ImportError:
    sys.stderr.write("ERROR: need `requests` and `pyyaml`. Run: pip install requests pyyaml\n")
    sys.exit(1)

PROMPT = (
    "Describe this image accurately and objectively. Include all visible "
    "subjects, objects, charts, diagrams, and any text within the image. "
    "Be specific and factual — this description will be read by a text-only LLM."
)


def load_vision_config(config_path, model_name):
    """Read a vision model entry from the proxy's config.yaml."""
    with open(config_path, encoding="utf-8") as fh:
        cfg = yaml.safe_load(fh)
    for m in cfg.get("models", []):
        if m.get("name") == model_name:
            return {
                "backend": m["backend"].rstrip("/"),
                "model": m.get("model") or model_name,
                "api_key": m.get("api_key"),
            }
    raise SystemExit(f"model {model_name!r} not found in {config_path}")


def content_list_blocks(mineru_dir):
    """Parse *_content_list_v2.json into (blocks, page_lookup).

    Returns a flat list of (page_idx, block) tuples. v2 is grouped by page as
    a list-of-lists (page_idx is the outer index); v1 is flat with a top-level
    page_idx field.
    """
    p = Path(mineru_dir)
    files = sorted(p.glob("*_content_list_v2.json")) or sorted(p.glob("*_content_list.json"))
    if not files:
        return None
    data = json.loads(files[0].read_text(encoding="utf-8"))
    if isinstance(data, list) and data and isinstance(data[0], list):
        return [(pidx, b) for pidx, page in enumerate(data) for b in page]
    return [(b.get("page_idx", -1), b) for b in data]


def describe_image(vision, img_path, timeout=120):
    """Send one cropped image to the vision model, return the description text."""
    b64 = base64.b64encode(img_path.read_bytes()).decode("ascii")
    data_url = f"data:image/jpeg;base64,{b64}"
    payload = {
        "model": vision["model"],
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": PROMPT},
                {"type": "image_url", "image_url": {"url": data_url}},
            ],
        }],
        "max_tokens": 2000,
    }
    headers = {"Content-Type": "application/json"}
    if vision.get("api_key"):
        headers["Authorization"] = f"Bearer {vision['api_key']}"
    resp = requests.post(
        vision["backend"] + "/chat/completions",
        headers=headers, json=payload, timeout=timeout,
    )
    resp.raise_for_status()
    data = resp.json()
    choices = data.get("choices") or []
    if not choices:
        return ""
    return (choices[0].get("message") or {}).get("content", "") or ""


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--mineru-dir", default="out/compare/mineru/vlm",
                    help="MinerU result dir (from collect_mineru.py)")
    ap.add_argument("--config", default="config.yaml", help="proxy config.yaml")
    ap.add_argument("--vision-model", default="qwen-vl-max",
                    help="model name in config.yaml to use for image description")
    ap.add_argument("--limit", type=int, default=0,
                    help="only process first N image blocks (for a quick smoke test)")
    args = ap.parse_args()

    vision = load_vision_config(args.config, args.vision_model)
    flat = content_list_blocks(args.mineru_dir)
    if not flat:
        sys.stderr.write(f"no content_list found under {args.mineru_dir}\n")
        sys.exit(1)

    targets = [(pidx, b) for pidx, b in flat if b.get("type") in ("image", "chart")]
    print(f"{len(targets)} image/chart blocks in {args.mineru_dir}")
    if args.limit:
        targets = targets[: args.limit]

    mineru_dir = Path(args.mineru_dir)
    out_path = mineru_dir / "images_descriptions.jsonl"
    n_ok = 0
    with open(out_path, "w", encoding="utf-8") as out:
        for i, (page_idx, b) in enumerate(targets, 1):
            c = b.get("content") or {}
            img_rel = (c.get("image_source") or {}).get("path", "")
            img_path = mineru_dir / img_rel if img_rel else None
            btype = b.get("type")
            if img_path is None or not img_path.exists():
                rec = {"page_idx": page_idx, "type": btype, "img_path": img_rel,
                       "caption": "", "description": "", "error": "image file missing"}
                out.write(json.dumps(rec, ensure_ascii=False) + "\n")
                print(f"[{i}/{len(targets)}] MISSING {img_rel}", file=sys.stderr)
                continue
            # caption: v2 content dict -> image_caption/chart_caption (span list)
            cap_key = "image_caption" if btype == "image" else "chart_caption"
            caption = c.get(cap_key) or []
            if caption and isinstance(caption, list):
                caption = " ".join(
                    (s.get("content", "") if isinstance(s, dict) else str(s))
                    for s in caption if s
                )
            try:
                desc = describe_image(vision, img_path)
            except Exception as e:
                rec = {"page_idx": page_idx, "type": btype, "img_path": img_rel,
                       "caption": caption, "description": "", "error": str(e)}
                out.write(json.dumps(rec, ensure_ascii=False) + "\n")
                print(f"[{i}/{len(targets)}] ERROR {img_rel}: {e}", file=sys.stderr)
                continue
            rec = {"page_idx": page_idx, "type": btype, "img_path": img_rel,
                   "caption": caption, "description": desc, "error": ""}
            out.write(json.dumps(rec, ensure_ascii=False) + "\n")
            n_ok += 1 if desc.strip() else 0
            print(f"[{i}/{len(targets)}] {btype} p{page_idx} {img_rel} "
                  f"desc={len(desc)} chars")

    print(f"done: {n_ok}/{len(targets)} described -> {out_path}")


if __name__ == "__main__":
    main()
