#!/usr/bin/env python3
"""Post-deploy probe for the claude-cli provider: system field + prompt cache.

Usage: verify_system_field.py [--base-url http://localhost:8081] [--model claude-sonnet-5-5]

1. system field: a secret word sits in the top-level `system`; the model must know it.
2. prompt cache: the same request (large system prompt, unique per run) is sent twice;
   the second one must read the prefix from cache (cache_read_input_tokens > 0).
Uses the subscription: about 3 short requests, the large one ~6-8k tokens.
"""
import argparse
import json
import sys
import time
import urllib.request
import uuid


def post(base, body, timeout=170):
    req = urllib.request.Request(
        base + "/v1/messages", data=json.dumps(body).encode(),
        headers={"content-type": "application/json", "anthropic-version": "2023-06-01"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.load(resp)


def text_of(resp):
    return "".join(b.get("text", "") for b in resp.get("content", []))


def usage_of(resp):
    u = resp.get("usage", {})
    return {k: u.get(k) for k in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "output_tokens")}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://localhost:8081")
    ap.add_argument("--model", default="claude-sonnet-5-5")
    args = ap.parse_args()
    base = args.base_url.rstrip("/")
    ok = True

    word = "ZEBRA-" + uuid.uuid4().hex[:6].upper()
    resp = post(base, {
        "model": args.model, "max_tokens": 40,
        "system": [{"type": "text", "text": f"Secret word: {word}. When asked for the secret word, answer with it only."}],
        "messages": [{"role": "user", "content": "What is the secret word? If you do not know, answer: UNKNOWN"}]})
    answer = text_of(resp)
    passed = word in answer
    ok &= passed
    print(f"[system field] expected {word!r}, got {answer.strip()!r} -> {'OK' if passed else 'FAIL (system is still dropped)'}")

    nonce = uuid.uuid4().hex
    filler = "\n".join(f"Rule {i}: keep answers short; reference id {nonce}-{i}; never repeat earlier rules." for i in range(420))
    body = {"model": args.model, "max_tokens": 20,
            "system": [{"type": "text", "text": f"Cache probe {nonce}.\n{filler}"}],
            "messages": [{"role": "user", "content": "Reply with the single word: ok"}]}
    first = post(base, body)
    time.sleep(2)
    second = post(base, body)
    u1, u2 = usage_of(first), usage_of(second)
    print("[cache] request 1:", u1)
    print("[cache] request 2:", u2)
    read2 = u2.get("cache_read_input_tokens") or 0
    passed = read2 > 0
    ok &= passed
    print(f"[cache] second request read {read2} tokens from cache -> {'OK' if passed else 'FAIL (no cache read)'}")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
