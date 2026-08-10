#!/usr/bin/env bash
# Collect go-llm-proxy's PDF-processing output for sample.pdf by routing a real
# HTTP request through a throwaway proxy instance whose backend is the local
# mock OpenAI server. The mock records the exact forwarded request (containing
# the pipeline's <pdf_content>), which is the ground-truth input the backend
# LLM would have received.
#
# Two request protocols are exercised to match how real clients send PDFs:
#   --protocol openai     OpenAI Chat Completions (image_url w/ pdf data-url)
#   --protocol anthropic  Anthropic Messages (document block) — what Claude Code sends
# Default: both.
#
# Usage: bash collect_proxy.sh [--protocol openai|anthropic|all] [project_root]
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Resolve the repo root: scripts/compare/test → repo root is three levels up.
ROOT="$(cd -- "$SCRIPT_DIR/../../.." && pwd)"
# Optional explicit root override.
if [[ "${2:-}" != "" ]]; then
  ROOT="$(cd -- "$2" && pwd)"
fi
PROTOCOL="${1:-all}"

MOCK_PORT=18887
PROXY_PORT=8788
OUT_DIR="$ROOT/out/compare"
CAPTURED="$OUT_DIR/captured.jsonl"
REQ_FILE="$OUT_DIR/pdf_request.json"
PYTHON="${PYTHON:-python}"
AUTH="Authorization: Bearer sk-local-dev-9f3a2c"

mkdir -p "$OUT_DIR"
cd "$ROOT"

# --- service lifecycle helpers -------------------------------------------
start_services() {
  rm -f "$CAPTURED" "$REQ_FILE"
  "$PYTHON" "$SCRIPT_DIR/mock_openai.py" --port "$MOCK_PORT" --outfile "$CAPTURED" &
  MOCK_PID=$!
  "$ROOT/go-llm-proxy.exe" -config "$ROOT/test/compare/proxy-test.yaml" &
  PROXY_PID=$!
  echo "[collect_proxy] mock pid=$MOCK_PID, proxy pid=$PROXY_PID"
}

wait_ready() {
  local port="$1" attempts="$2"
  local i=0
  while (( i < attempts )); do
    if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$port/v1/models" ; then
      return 0
    fi
    i=$((i+1)); sleep 0.5
  done
  return 1
}

stop_services() {
  kill "$MOCK_PID" "$PROXY_PID" 2>/dev/null || true
  # give them a beat, then hard-kill if still around
  sleep 1
  kill -9 "$MOCK_PID" "$PROXY_PID" 2>/dev/null || true
}

send_and_capture() {
  local protocol="$1"
  local endpoint
  if [[ "$protocol" == "anthropic" ]]; then
    endpoint="/v1/messages"
  else
    endpoint="/v1/chat/completions"
  fi

  rm -f "$CAPTURED"
  "$PYTHON" "$SCRIPT_DIR/build_pdf_request.py" \
    --pdf "$ROOT/sample.pdf" \
    --model test-pdf-model \
    --protocol "$protocol" \
    --out "$REQ_FILE"

  HTTP_CODE=$(curl -s -o "$OUT_DIR/proxy_http_response.json" -w "%{http_code}" \
    -X POST "http://127.0.0.1:$PROXY_PORT$endpoint" \
    -H "Content-Type: application/json" \
    -H "$AUTH" \
    --data-binary "@$REQ_FILE")
  echo "[collect_proxy] $protocol -> $endpoint HTTP $HTTP_CODE"
  sleep 1
}

# --- collect for each requested protocol ----------------------------------
collect_one() {
  local protocol="$1"
  local outfile="$OUT_DIR/go_llm_proxy_output_${protocol}.txt"
  rm -f "$outfile"

  start_services
  trap 'stop_services' EXIT

  if ! wait_ready "$MOCK_PORT" 30; then
    echo "[collect_proxy] ERROR: mock not ready on :$MOCK_PORT" >&2
    exit 1
  fi
  if ! wait_ready "$PROXY_PORT" 60; then
    echo "[collect_proxy] ERROR: proxy not ready on :$PROXY_PORT" >&2
    exit 1
  fi
  echo "[collect_proxy] mock + proxy ready"

  send_and_capture "$protocol"

  if [[ ! -s "$CAPTURED" ]]; then
    echo "[collect_proxy] ERROR: no capture for $protocol" >&2
    exit 1
  fi

  "$PYTHON" - "$CAPTURED" "$outfile" "$protocol" <<'PY'
import json, sys
jsonl, outfile, protocol = sys.argv[1], sys.argv[2], sys.argv[3]
blocks = []
with open(jsonl, encoding="utf-8") as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        rec = json.loads(line)
        try:
            req = json.loads(rec["body"])
        except Exception:
            continue
        for msg in req.get("messages", []):
            content = msg.get("content")
            if isinstance(content, str):
                if "<pdf_content" in content:
                    blocks.append(content)
            elif isinstance(content, list):
                for part in content:
                    if isinstance(part, dict) and part.get("type") == "text":
                        t = part.get("text", "")
                        if "<pdf_content" in t:
                            blocks.append(t)
                    elif isinstance(part, dict) and part.get("type") == "pdf_data":
                        blocks.append("[pdf_data NOT replaced by pipeline]")
if not blocks:
    print(f"WARN: no <pdf_content> block for {protocol}", file=sys.stderr)
with open(outfile, "w", encoding="utf-8") as fh:
    fh.write("\n\n".join(blocks) + ("\n" if blocks else ""))
print(f"[collect_proxy] {protocol}: extracted {len(blocks)} block(s) -> {outfile}")
PY

  stop_services
  trap - EXIT
}

case "$PROTOCOL" in
  all)   collect_one openai; collect_one anthropic ;;
  openai)    collect_one openai ;;
  anthropic) collect_one anthropic ;;
  *) echo "unknown protocol: $PROTOCOL" >&2; exit 1 ;;
esac

echo "[collect_proxy] done."
