#!/usr/bin/env python3
"""Build the Chat Completions request body that carries sample.pdf, and helpers
to extract the pipeline's injected output from the mock's captured jsonl.

Pure stdlib. Usage:
    # emit request body JSON to stdout (or --out):
    python build_pdf_request.py --pdf sample.pdf --model test-pdf-model

    # extract the final forwarded content from a captured jsonl:
    python build_pdf_request.py --extract out/compare/captured.jsonl
"""
import argparse
import base64
import json
import sys


def build_request(pdf_path, model, system_text=None, protocol="openai"):
    """Build a request carrying sample.pdf.

    protocol="openai":   OpenAI Chat Completions — PDF as an image_url part with a
                         data:application/pdf;base64,... URL (normalized to pdf_data
                         by the proxy, matching the ChatGPT/OpenCode convention).
    protocol="anthropic": Anthropic Messages — PDF as a document block
                         ({type: document, source:{type: base64, media_type: application/pdf}}),
                         the shape Claude Code actually sends.
    """
    with open(pdf_path, "rb") as fh:
        b64 = base64.b64encode(fh.read()).decode("ascii")
    messages = []
    if system_text:
        messages.append({"role": "system", "content": system_text})
    if protocol == "anthropic":
        messages.append({
            "role": "user",
            "content": [{
                "type": "document",
                "source": {"type": "base64", "media_type": "application/pdf", "data": b64},
                "title": "sample.pdf",
            }],
        })
        return {"model": model, "max_tokens": 64, "messages": messages}
    # openai
    data_url = f"data:application/pdf;base64,{b64}"
    content = [{"type": "image_url", "image_url": {"url": data_url}}]
    messages.append({"role": "user", "content": content})
    return {"model": model, "messages": messages, "max_tokens": 64}


def extract_captured(jsonl_path):
    """Read the mock's captured jsonl and return the forwarded request bodies."""
    records = []
    with open(jsonl_path, "r", encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            rec = json.loads(line)
            try:
                rec["parsed"] = json.loads(rec["body"])
            except Exception:
                rec["parsed"] = None
            records.append(rec)
    return records


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--pdf", default="sample.pdf")
    ap.add_argument("--model", default="test-pdf-model")
    ap.add_argument("--protocol", choices=["openai", "anthropic"], default="openai",
                    help="request protocol shape (openai image_url vs anthropic document block)")
    ap.add_argument("--out", default=None, help="write request JSON to file")
    ap.add_argument("--extract", metavar="JSONL", help="extract forwarded requests from a captured jsonl")
    args = ap.parse_args()

    if args.extract:
        records = extract_captured(args.extract)
        if not records:
            sys.stderr.write("no captured records\n")
            sys.exit(1)
        for i, rec in enumerate(records):
            parsed = rec.get("parsed") or {}
            msgs = parsed.get("messages") or []
            print(f"=== captured #{i} path={rec['path']} messages={len(msgs)} ===")
            print(json.dumps(parsed, ensure_ascii=False, indent=2))
        return

    req = build_request(args.pdf, args.model, protocol=args.protocol)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(req, fh, ensure_ascii=False)
        sys.stderr.write(f"request written to {args.out}\n")
    else:
        print(json.dumps(req, ensure_ascii=False))


if __name__ == "__main__":
    main()
