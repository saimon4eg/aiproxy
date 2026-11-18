#!/usr/bin/env python3
"""Codex-like Responses API client: smoke matrix for the native converters.

Sends requests shaped like the ones Codex CLI sends to /v1/responses (input
array, instructions, reasoning, function tools, MCP namespace tools, streaming,
multi-turn with function_call_output and reasoning replay) and checks the reply.

    python3 scripts/codex_like_client.py --base http://127.0.0.1:8082 --model deepseek-deepseek-v4-flash

Results are printed as a matrix; unreached cells are reported as "skip", never
as "fail". This is an emulator, NOT the real Codex.
"""
import argparse
import json
import sys
import urllib.error
import urllib.request

TOOL = {
    "type": "function", "name": "get_weather", "description": "Get the weather for a city.",
    "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
    "strict": False,
}
NS_TOOL = {
    "type": "namespace", "name": "mcp__demo__", "description": "demo server",
    "tools": [{"type": "function", "name": "get_time", "description": "Get time in a city.",
               "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}],
}


def user(text):
    return {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}


def post(base, body, stream=False, timeout=180):
    req = urllib.request.Request(base + "/v1/responses", data=json.dumps(body).encode(),
                                 headers={"content-type": "application/json"})
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    if not stream:
        return resp.status, json.loads(resp.read())
    events = []
    ev_type = None
    for raw in resp:
        line = raw.decode().rstrip("\r\n")
        if line.startswith("event:"):
            ev_type = line[6:].strip()
        elif line.startswith("data:"):
            events.append((ev_type, json.loads(line[5:].strip())))
    return resp.status, events


def out_items(resp, typ):
    return [i for i in resp.get("output", []) if i.get("type") == typ]


def text_of(resp):
    return "".join(c.get("text", "") for m in out_items(resp, "message") for c in m.get("content", []))


def final_response(events):
    for t, d in reversed(events):
        if t in ("response.completed", "response.incomplete", "response.failed"):
            return t, d["response"]
    return None, None


def check_stream_shape(events):
    errs = []
    seqs = [d.get("sequence_number") for _, d in events]
    if seqs != list(range(len(seqs))):
        errs.append("sequence_number not 0..n")
    if not events or events[0][0] != "response.created":
        errs.append("first event is not response.created")
    t, _ = final_response(events)
    if t is None:
        errs.append("no terminal event")
    return errs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8082")
    ap.add_argument("--model", required=True)
    ap.add_argument("--no-thinking", action="store_true", help="skip reasoning cells (model without thinking)")
    a = ap.parse_args()
    base, model = a.base, a.model
    results = []

    def cell(name, fn):
        try:
            ok, note = fn()
        except Exception as e:  # noqa: BLE001
            ok, note = False, f"exception: {e}"
        results.append((name, ok, note))
        print(f"{'PASS' if ok else 'FAIL'}  {name:34} {note}", flush=True)

    base_req = {"model": model, "max_output_tokens": 600}

    def non_stream_text():
        s, r = post(base, dict(base_req, input=[user("Reply with exactly: pong")], stream=False))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        return ("pong" in text_of(r).lower() and r["status"] == "completed" and r["usage"]["total_tokens"] > 0,
                f"text={text_of(r)[:40]!r} usage={r['usage']['total_tokens']}")

    def stream_text():
        s, ev = post(base, dict(base_req, input=[user("Reply with exactly: pong")], stream=True), stream=True)
        if s != 200:
            return False, f"HTTP {s}: {str(ev)[:160]}"
        errs = check_stream_shape(ev)
        t, r = final_response(ev)
        deltas = "".join(d.get("delta", "") for ty, d in ev if ty == "response.output_text.delta")
        ok = not errs and t == "response.completed" and "pong" in deltas.lower() and "pong" in text_of(r).lower()
        return ok, f"{len(ev)} events, final={t}, deltas={deltas[:30]!r} {errs}"

    def instructions():
        s, r = post(base, dict(base_req, instructions="Always answer with the single word BANANA and nothing else.",
                               input=[user("What is the capital of France?")]))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        return "banana" in text_of(r).lower(), f"text={text_of(r)[:60]!r}"

    def tool_call_non_stream():
        s, r = post(base, dict(base_req, tools=[TOOL], tool_choice="auto",
                               input=[user("What is the weather in Paris? Use the tool.")]))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        fc = out_items(r, "function_call")
        ok = bool(fc) and fc[0]["name"] == "get_weather" and "paris" in fc[0]["arguments"].lower()
        return ok, f"calls={[(c['name'], c['arguments']) for c in fc]}"

    def tool_call_stream():
        s, ev = post(base, dict(base_req, tools=[TOOL], tool_choice="auto", stream=True,
                                input=[user("What is the weather in Paris? Use the tool.")]), stream=True)
        if s != 200:
            return False, f"HTTP {s}: {str(ev)[:160]}"
        t, r = final_response(ev)
        fc = out_items(r, "function_call") if r else []
        argdeltas = "".join(d.get("delta", "") for ty, d in ev if ty == "response.function_call_arguments.delta")
        done = [d["arguments"] for ty, d in ev if ty == "response.function_call_arguments.done"]
        ok = (not check_stream_shape(ev) and bool(fc) and done and json.loads(done[0]).get("city", "").lower() == "paris"
              and argdeltas == done[0])
        return ok, f"final={t} args={done[:1]}"

    def multi_turn_tool_roundtrip():
        s, r = post(base, dict(base_req, tools=[TOOL], tool_choice="auto",
                               input=[user("What is the weather in Paris? Use the tool.")]))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        fc = out_items(r, "function_call")
        if not fc:
            return False, "no tool call in first turn"
        history = [user("What is the weather in Paris? Use the tool.")] + r["output"] + [
            {"type": "function_call_output", "call_id": fc[0]["call_id"], "output": "Sunny, 21C"}]
        s2, r2 = post(base, dict(base_req, tools=[TOOL], input=history))
        if s2 != 200:
            return False, f"HTTP {s2}: {str(r2)[:200]}"
        t = text_of(r2)
        return ("21" in t or "sunny" in t.lower()), f"answer={t[:80]!r}"

    def reasoning_replay():
        if a.no_thinking:
            return True, "skip (--no-thinking)"
        body = dict(base_req, tools=[TOOL], tool_choice="auto", reasoning={"effort": "high", "summary": "auto"},
                    include=["reasoning.encrypted_content"], max_output_tokens=2000,
                    input=[user("What is the weather in Paris? Use the tool.")])
        s, r = post(base, body)
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        fc = out_items(r, "function_call")
        rs = out_items(r, "reasoning")
        if not fc:
            return False, "no tool call"
        history = [user("What is the weather in Paris? Use the tool.")] + r["output"] + [
            {"type": "function_call_output", "call_id": fc[0]["call_id"], "output": "Sunny, 21C"}]
        s2, r2 = post(base, dict(body, tool_choice="auto", input=history))
        if s2 != 200:
            return False, f"replay HTTP {s2}: {str(r2)[:240]}"
        return True, f"reasoning items={len(rs)} (signature={'yes' if rs and rs[0].get('encrypted_content') else 'no'}), replay ok"

    def reasoning_stream():
        if a.no_thinking:
            return True, "skip (--no-thinking)"
        s, ev = post(base, dict(base_req, max_output_tokens=2000, stream=True, reasoning={"effort": "high", "summary": "auto"},
                                input=[user("What is 17*23? Think, then answer briefly.")]), stream=True)
        if s != 200:
            return False, f"HTTP {s}: {str(ev)[:160]}"
        t, r = final_response(ev)
        kinds = sorted({ty for ty, _ in ev if "reasoning" in (ty or "")})
        return (not check_stream_shape(ev) and t == "response.completed" and "391" in text_of(r),
                f"final={t} reasoning_events={kinds} answer={text_of(r)[:30]!r}")

    def mcp_namespace():
        s, r = post(base, dict(base_req, tools=[NS_TOOL], tool_choice="auto",
                               input=[user("What time is it in Tokyo? Use the tool.")]))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        fc = out_items(r, "function_call")
        ok = bool(fc) and fc[0]["name"] == "get_time" and fc[0].get("namespace") == "mcp__demo__"
        return ok, f"call={[(c['name'], c.get('namespace')) for c in fc]}"

    def max_tokens_incomplete():
        s, r = post(base, dict(base_req, max_output_tokens=16, input=[user("Write a long essay about the sea.")]))
        if s != 200:
            return False, f"HTTP {s}: {str(r)[:160]}"
        return (r["status"] == "incomplete" and r["incomplete_details"]["reason"] == "max_output_tokens",
                f"status={r['status']} details={r.get('incomplete_details')}")

    def bad_request():
        s, r = post(base, {"model": model})
        return s == 400, f"HTTP {s}: {str(r)[:100]}"

    def client_cancel():
        # Open a stream and drop it after the first event: the server must not hang or crash.
        req = urllib.request.Request(base + "/v1/responses", data=json.dumps(dict(
            base_req, stream=True, max_output_tokens=2000, input=[user("Count from 1 to 200, one number per line.")])).encode(),
            headers={"content-type": "application/json"})
        resp = urllib.request.urlopen(req, timeout=60)
        resp.readline()
        resp.close()
        s, r = post(base, dict(base_req, input=[user("Reply with exactly: pong")]))
        return s == 200, "server alive after client disconnect"

    for name, fn in [
        ("text / non-stream", non_stream_text), ("text / stream", stream_text),
        ("instructions -> system", instructions),
        ("tools / non-stream", tool_call_non_stream), ("tools / stream", tool_call_stream),
        ("tools / multi-turn roundtrip", multi_turn_tool_roundtrip),
        ("reasoning / replay across turns", reasoning_replay), ("reasoning / stream", reasoning_stream),
        ("mcp namespace tool", mcp_namespace), ("max_output_tokens -> incomplete", max_tokens_incomplete),
        ("invalid request -> 400", bad_request), ("client disconnect mid-stream", client_cancel),
    ]:
        cell(name, fn)

    failed = [r for r in results if not r[1]]
    print(f"\n{len(results) - len(failed)}/{len(results)} passed (emulator, not real Codex)")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
