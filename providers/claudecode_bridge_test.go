package providers

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Reader and writer both closing must not panic. The non-streaming converter
// closes the body itself, and the handler closes the writer when it returns;
// with a separate sync.Once on each side the second close panicked the whole
// request with "close of closed channel".
func TestBytePipeDoubleCloseIsSafe(t *testing.T) {
	reader, writer := newBytePipe()

	if _, err := writer.Write([]byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("reader close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer close after reader close: %v", err)
	}
	// Closing again, in either order, stays harmless.
	if err := reader.Close(); err != nil {
		t.Fatalf("second reader close: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second writer close: %v", err)
	}

	buf := make([]byte, 32)
	n, err := reader.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read after close: %v", err)
	}
	if got := string(buf[:n]); got != "payload" {
		t.Fatalf("buffered payload lost: %q", got)
	}
	if _, err := reader.Read(buf); err != io.EOF {
		t.Fatalf("read after drain = %v, want EOF", err)
	}
}

// A reader that abandons the pipe must unblock the writer instead of leaking the
// handler goroutine (and the CLI process behind it). The pipe buffers pipeDepth
// chunks, so the writer has to overrun that to block at all.
func TestBytePipeAbandonedReaderUnblocksWriter(t *testing.T) {
	reader, writer := newBytePipe()
	reader.Close()

	done := make(chan error, 1)
	go func() {
		for i := 0; i < pipeDepth*4; i++ {
			if _, err := writer.Write([]byte("chunk")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writer succeeded although the reader was gone")
		}
	case <-timeoutAfterSeconds(2):
		t.Fatal("writer stayed blocked after the reader closed")
	}
}

// The Responses/Codex converter must reach the CLI, not api.anthropic.com.
func TestResponsesBridgeUsesClaudeCLI(t *testing.T) {
	upstream := &upstreamRecorder{}
	server := httptest.NewServer(upstream.handler())
	defer server.Close()

	p, _ := testProvider(t, server.URL, map[string]any{"mode": "text", "upstream_calls": 1})
	p.ConvertToResponses = true

	body := []byte(`{"model":"claude-sonnet-5-5","stream":false,
	 "input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	p.makeResponsesToAnthropicHandler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if upstream.count() != 1 {
		t.Fatalf("the CLI's upstream leg ran %d times, want 1", upstream.count())
	}
	if got := upstream.paths; len(got) == 0 || got[0] != "/v1/messages" {
		t.Fatalf("upstream path %v, want /v1/messages", got)
	}
}

func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}
