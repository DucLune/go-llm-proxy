#!/usr/bin/env bash
# One-shot pipeline for the go-llm-proxy × MinerU PDF comparison.
#
# Usage:
#   MINERU_API_TOKEN=your_token bash run_all.sh [project_root]
#
# Requires:
#   - requests (pip install requests)
#   - a valid MINERU_API_TOKEN (create at mineru.net API management page)
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
if [[ "${1:-}" != "" ]]; then
  ROOT="$(cd -- "$1" && pwd)"
fi
OUT_DIR="$ROOT/out/compare"
PDFINFO="$OUT_DIR/pdfinfo.json"
PDFINFO_BIN="$(command -v pdfinfo || echo '/d/Program Files (x86)/texlive/2024/bin/windows/pdfinfo')"
PDFIMAGES_BIN="$(command -v pdfimages || echo '/d/Program Files (x86)/texlive/2024/bin/windows/pdfimages')"

mkdir -p "$OUT_DIR"
cd "$ROOT"

echo "==================== [1/4] pdfinfo snapshot ===================="
python - "$PDFINFO" "$PDFIMAGES" <<'PY'
import json, re, subprocess, sys
pdfinfo, pdfimages, out = sys.argv[1], sys.argv[2], "out/compare/pdfinfo.json"
info = {"file": "sample.pdf"}
try:
    # pdfinfo may emit GBK-encoded CJK metadata (e.g. Chinese standard time);
    # decode leniently.
    r = subprocess.run([pdfinfo, "sample.pdf"], capture_output=True, text=True,
                       errors="replace", timeout=30)
    for line in r.stdout.splitlines():
        if ":" in line:
            k, v = line.split(":", 1)
            info[k.strip()] = v.strip()
except Exception as e:
    info["pdfinfo_error"] = str(e)
try:
    r = subprocess.run([pdfimages, "-list", "sample.pdf"], capture_output=True, text=True, timeout=30)
    imgs = [l for l in r.stdout.splitlines()[2:] if l.strip() and not l.strip().startswith("-")]
    info["images"] = len(imgs)
except Exception as e:
    info["images"] = 0
    info["pdfimages_error"] = str(e)
with open(out, "w", encoding="utf-8") as fh:
    json.dump(info, fh, ensure_ascii=False, indent=2)
print(f"pdfinfo snapshot -> {out}")
print("  pages:", info.get("Pages"), "| producer:", info.get("Producer"), "| images:", info.get("images"))
PY

echo "==================== [2/4] collect go-llm-proxy output ===================="
bash "$SCRIPT_DIR/collect_proxy.sh" all

echo "==================== [3/4] collect MinerU cloud output ===================="
python "$SCRIPT_DIR/collect_mineru.py" --pdf "$ROOT/sample.pdf" --out "$OUT_DIR/mineru" --model pipeline --model vlm

echo "==================== [4/4] build comparison report ===================="
python "$SCRIPT_DIR/compare.py" --out "$OUT_DIR" --pdfinfo "$PDFINFO"

echo ""
echo "==================== DONE ===================="
echo "report: $OUT_DIR/report.md"
echo "proxy outputs: $OUT_DIR/go_llm_proxy_output_{openai,anthropic}.txt"
echo "mineru outputs: $OUT_DIR/mineru/{pipeline,vlm}/"
