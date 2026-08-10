#!/usr/bin/env python3
"""Collect MinerU cloud API parsing output for sample.pdf.

Implements the verified v4 API flow from https://mineru.net/apiManage/docs:
  1. POST /api/v4/file-urls/batch  → get batch_id + signed upload URLs
  2. PUT <file_url>                → upload sample.pdf (no Content-Type header)
  3. GET /api/v4/extract-results/batch/{batch_id} → poll until done
  4. GET full_zip_url              → download and extract the result zip

One run per model_version (pipeline / vlm). Output lands under out/compare/mineru/.

Dependencies: `requests` (pip install requests). Auth via MINERU_API_TOKEN env var.
"""
import argparse
import json
import os
import sys
import time
import zipfile
from pathlib import Path

try:
    import requests
except ImportError:
    sys.stderr.write("ERROR: `requests` not installed. Run: pip install requests\n")
    sys.exit(1)

API = "https://mineru.net"
UPLOAD_SUBMIT = f"{API}/api/v4/file-urls/batch"
RESULT_POLL = f"{API}/api/v4/extract-results/batch/"

# state values per docs
TERMINAL = {"done", "failed"}
POLL_INTERVAL = 5          # seconds between polls
POLL_TIMEOUT = 30 * 60     # overall timeout (30 min)


def get_token():
    tok = os.environ.get("MINERU_API_TOKEN")
    if not tok:
        # Fall back to the repo's .env (MINERU_APIKEY), avoiding shell history exposure.
        # __file__ = test/compare/scripts/collect_mineru.py → repo root is parents[3].
        env_file = Path(__file__).resolve().parents[3] / ".env"
        if env_file.exists():
            for line in env_file.read_text(encoding="utf-8").splitlines():
                line = line.strip()
                if line.startswith("MINERU_APIKEY="):
                    tok = line.split("=", 1)[1].strip().strip('"').strip("'")
                    break
    if not tok:
        sys.stderr.write(
            "ERROR: set MINERU_API_TOKEN (env) or MINERU_APIKEY in .env "
            "(create it at mineru.net API management page)\n"
        )
        sys.exit(1)
    return tok


def api_headers(token):
    return {"Authorization": f"Bearer {token}", "Accept": "*/*"}


def submit_upload(token, model_version):
    """Step 1: request a signed upload URL. Returns (batch_id, file_url)."""
    body = {
        "files": [{"name": "sample.pdf", "data_id": f"sample-{model_version}"}],
        "model_version": model_version,
    }
    resp = requests.post(
        UPLOAD_SUBMIT, headers=api_headers(token), json=body, timeout=60
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("code") != 0:
        sys.stderr.write(f"submit failed: {json.dumps(data, ensure_ascii=False)}\n")
        sys.exit(1)
    batch_id = data["data"]["batch_id"]
    urls = data["data"]["file_urls"]
    if not urls:
        sys.stderr.write("no upload URL returned\n")
        sys.exit(1)
    return batch_id, urls[0]


def upload_file(file_url, pdf_path):
    """Step 2: PUT the file to the signed URL. Docs: no Content-Type header."""
    with open(pdf_path, "rb") as fh:
        data = fh.read()
    resp = requests.put(file_url, data=data, timeout=120)
    if resp.status_code >= 300:
        sys.stderr.write(f"upload failed: HTTP {resp.status_code}\n")
        sys.exit(1)


def poll_result(token, batch_id):
    """Step 3: poll until terminal. Returns the per-file result dict."""
    deadline = time.time() + POLL_TIMEOUT
    while time.time() < deadline:
        resp = requests.get(
            f"{RESULT_POLL}{batch_id}", headers=api_headers(token), timeout=60
        )
        resp.raise_for_status()
        data = resp.json()
        if data.get("code") != 0:
            sys.stderr.write(f"poll error: {json.dumps(data, ensure_ascii=False)}\n")
            sys.exit(1)
        results = data["data"]["extract_result"]
        # take the one matching sample.pdf
        target = next((r for r in results if r.get("file_name") == "sample.pdf"), results[0])
        state = target.get("state")
        prog = target.get("extract_progress") or {}
        print(
            f"  state={state} pages={prog.get('extracted_pages', '-')}/{prog.get('total_pages', '-')}",
            flush=True,
        )
        if state in TERMINAL:
            return target
        time.sleep(POLL_INTERVAL)
    sys.stderr.write(f"poll timeout after {POLL_TIMEOUT}s\n")
    sys.exit(1)


def download_and_extract(full_zip_url, dest_dir):
    """Step 4: download the result zip and extract into dest_dir."""
    Path(dest_dir).mkdir(parents=True, exist_ok=True)
    zip_path = Path(dest_dir) / "result.zip"
    resp = requests.get(full_zip_url, timeout=120)
    resp.raise_for_status()
    zip_path.write_bytes(resp.content)
    with zipfile.ZipFile(zip_path) as zf:
        zf.extractall(dest_dir)
    zip_path.unlink(missing_ok=True)
    print(f"  extracted {len(list(Path(dest_dir).rglob('*')))} files -> {dest_dir}")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--pdf", default="sample.pdf", help="path to the PDF to upload")
    ap.add_argument("--out", default="out/compare/mineru", help="output root dir")
    ap.add_argument(
        "--model",
        choices=["pipeline", "vlm", "MinerU-HTML"],
        action="append",
        help="model_version(s) to run; repeat for multiple (default: pipeline vlm)",
    )
    ap.add_argument("--token", default=None, help="override MINERU_API_TOKEN")
    args = ap.parse_args()

    token = args.token or get_token()
    models = args.model or ["pipeline", "vlm"]
    pdf_path = Path(args.pdf)
    if not pdf_path.exists():
        sys.stderr.write(f"PDF not found: {pdf_path}\n")
        sys.exit(1)

    api_log = []
    for mv in models:
        print(f"=== MinerU cloud API: model_version={mv} ===", flush=True)
        batch_id, file_url = submit_upload(token, mv)
        print(f"  batch_id={batch_id}")
        api_log.append({"model_version": mv, "batch_id": batch_id, "file_url": file_url})
        upload_file(file_url, pdf_path)
        print("  uploaded sample.pdf")
        target = poll_result(token, batch_id)
        api_log[-1].update(target)
        if target.get("state") == "failed":
            sys.stderr.write(f"  FAILED: {target.get('err_msg')}\n")
            continue
        full_zip_url = target.get("full_zip_url")
        if not full_zip_url:
            sys.stderr.write("  no full_zip_url in result\n")
            continue
        download_and_extract(full_zip_url, args.out / Path(mv))

    log_path = Path(args.out) / "api_log.jsonl"
    Path(args.out).mkdir(parents=True, exist_ok=True)
    with open(log_path, "w", encoding="utf-8") as fh:
        for entry in api_log:
            fh.write(json.dumps(entry, ensure_ascii=False) + "\n")
    print(f"api log -> {log_path}")


if __name__ == "__main__":
    main()
