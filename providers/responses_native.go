package providers

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/whtsky/copilot2api/anthropic"
)

// writeAnthropicStreamAsResponses reads an Anthropic Messages SSE stream from
// body and writes it to w as a Responses API SSE stream, flushing per event.
// It always terminates the Responses stream (completed / incomplete / failed),
// even when the upstream stream is cut off, so clients never hang.
func writeAnthropicStreamAsResponses(w http.ResponseWriter, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	conv := anthropic.NewAnthropicToResponsesStream()

	write := func(events []anthropic.ResponsesStreamEvent) error {
		for _, ev := range events {
			if _, err := w.Write([]byte("event: " + ev.Type + "\ndata: ")); err != nil {
				return err
			}
			if _, err := w.Write(ev.Data); err != nil {
				return err
			}
			if _, err := w.Write([]byte("\n\n")); err != nil {
				return err
			}
		}
		if len(events) > 0 && flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// bufio.Reader (not Scanner): SSE lines carrying big tool-argument deltas
	// or signatures can exceed any fixed scanner buffer.
	r := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := r.ReadString('\n')
		if data, ok := sseData(line); ok {
			if werr := write(conv.Feed([]byte(data))); werr != nil {
				return werr
			}
			if conv.Done() {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return write(conv.Finish())
			}
			// Upstream read error: tell the client the response failed.
			if werr := write(conv.Finish()); werr != nil {
				return werr
			}
			return err
		}
	}
}

// sseData extracts the payload of an SSE "data:" line.
func sseData(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "), true
}
