#!/usr/bin/env python3
"""Bridge: uses linguafranca for Responses<->Anthropic conversion.
- 'req'  : Responses -> Anthropic request (stdin JSON, stdout JSON)
- 'resp' : Anthropic -> Responses response (stdin JSON, stdout JSON)
- 'stream': Anthropic SSE events -> Responses SSE events (stdin JSON lines, stdout JSON lines)
"""
import sys, json
import linguafranca as lf

def main():
    mode = sys.argv[1] if len(sys.argv) > 1 else "req"

    if mode == "req":
        data = json.load(sys.stdin)
        result = lf.convert_request_json(
            data,
            source_format=lf.FormatName.OPEN_RESPONSES,
            target_format=lf.FormatName.ANTHROPIC_MESSAGES
        )
        json.dump(result.value, sys.stdout)

    elif mode == "resp":
        data = json.load(sys.stdin)
        result = lf.convert_response_json(
            data,
            source_format=lf.FormatName.ANTHROPIC_MESSAGES,
            target_format=lf.FormatName.OPEN_RESPONSES
        )
        json.dump(result.value, sys.stdout)

    elif mode == "stream":
        stream = lf.convert_response_stream_json(
            (json.loads(line) for line in sys.stdin if line.strip()),
            source_format=lf.FormatName.ANTHROPIC_MESSAGES,
            target_format=lf.FormatName.OPEN_RESPONSES
        )
        for event in stream:
            print(json.dumps(event))

if __name__ == "__main__":
    main()
