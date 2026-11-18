package providers

// In-process bridge so a converting path (Responses today) drives the
// claude-cli transport instead of making its own HTTP call.
//
// The converter handlers were written against an upstream URL, so for a
// claude-cli provider they would talk to api.anthropic.com directly — bypassing
// the CLI and therefore the subscription entirely. This bridge feeds the
// converted request back into the provider's own handler and presents the result
// as an *http.Response, so those handlers keep their shape while the generation
// still happens through the CLI.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// pipeResponseWriter adapts an http.ResponseWriter onto a byte pipe.
type pipeResponseWriter struct {
	header http.Header
	status int
	wrote  bool
	ready  chan struct{}
	writer *bytePipeWriter
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	close(w.ready)
}

func (w *pipeResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(p)
}

// Flush is a no-op: the pipe is unbuffered, so every Write is already visible to
// the reader.
func (w *pipeResponseWriter) Flush() {}

// openClaudeMessages runs the claude-cli handler for one Anthropic-format body
// and returns the answer as a response whose Body streams incrementally.
//
// The caller must close the Body: closing it (or cancelling ctx) unblocks the
// handler goroutine, which would otherwise wait forever on an unread pipe and
// leak the Claude CLI process behind it.
func (p *ProviderConfig) openClaudeMessages(ctx context.Context, body []byte) (*http.Response, error) {
	reader, writer := newBytePipe()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	w := &pipeResponseWriter{header: make(http.Header), ready: make(chan struct{}), writer: writer}
	go func() {
		defer writer.Close()
		p.makeClaudeCLIHandler().ServeHTTP(w, req)
	}()

	select {
	case <-w.ready:
	case <-writer.closed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     w.header,
		Body:       reader,
	}, nil
}

// collectClaudeMessages runs the handler and buffers its JSON answer: the
// non-streaming path writes exactly one body, so buffering costs nothing.
func (p *ProviderConfig) collectClaudeMessages(ctx context.Context, body []byte) ([]byte, int, error) {
	recorder := httptest.NewRecorder()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages",
		bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	p.makeClaudeCLIHandler().ServeHTTP(recorder, req)
	return recorder.Body.Bytes(), recorder.Code, nil
}

// bytePipe is a small buffered byte channel with an explicit close, so a reader
// that stops consuming cannot wedge the writer forever.
//
// Reader and writer share ONE sync.Once for the close signal: closing the same
// channel from both sides is what panicked the response converter with
// "close of closed channel" on the non-streaming path.
type bytePipe struct {
	ch     chan []byte
	rest   []byte
	closed chan struct{}
	once   *sync.Once
}

type bytePipeWriter struct {
	ch     chan []byte
	closed chan struct{}
	once   *sync.Once
	chOnce sync.Once
}

// pipeDepth lets the handler run a few writes ahead of the reader without
// blocking, which keeps the common case cheap.
const pipeDepth = 8

func newBytePipe() (*bytePipe, *bytePipeWriter) {
	ch := make(chan []byte, pipeDepth)
	closed := make(chan struct{})
	once := &sync.Once{}
	return &bytePipe{ch: ch, closed: closed, once: once},
		&bytePipeWriter{ch: ch, closed: closed, once: once}
}

func (p *bytePipe) Read(b []byte) (int, error) {
	for len(p.rest) == 0 {
		select {
		case chunk, ok := <-p.ch:
			if !ok {
				return 0, io.EOF
			}
			p.rest = chunk
		case <-p.closed:
			// Abandoned: drain what is already buffered, then report EOF instead
			// of blocking the handler forever.
			select {
			case chunk, ok := <-p.ch:
				if !ok {
					return 0, io.EOF
				}
				p.rest = chunk
			default:
				return 0, io.EOF
			}
		}
	}
	n := copy(b, p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

// Close tells the writer the reader is gone so it unblocks with an error.
func (p *bytePipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (w *bytePipeWriter) Write(b []byte) (int, error) {
	chunk := make([]byte, len(b))
	copy(chunk, b)
	select {
	case w.ch <- chunk:
		return len(b), nil
	case <-w.closed:
		return 0, http.ErrBodyReadAfterClose
	}
}

func (w *bytePipeWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	w.chOnce.Do(func() { close(w.ch) })
	return nil
}
