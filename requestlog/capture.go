package requestlog

import "bytes"

// bodyCapture keeps the head and the tail of a body as it is written, dropping only
// the middle.
//
// A response is captured while it streams to the client, so its size is unknown in
// advance and it cannot be elided after the fact. Buffering the first N bytes alone
// throws away the tail — which for an SSE stream is exactly where `message_stop` and
// the token usage live, and for a Messages reply is where the answer ends. Keeping a
// bounded head and a bounded ring tail costs the same N bytes and shows both ends.
type bodyCapture struct {
	headMax, tailMax int
	head, tail       []byte
	total            int64
}

// newBodyCapture returns a capture whose rendered form stays within limit bytes.
func newBodyCapture(limit int) *bodyCapture {
	if limit < 1024 {
		limit = 1024
	}
	head := limit / 2
	return &bodyCapture{headMax: head, tailMax: limit - head}
}

// Write records p, filling the head first and then the tail ring.
func (c *bodyCapture) Write(p []byte) {
	c.total += int64(len(p))
	if room := c.headMax - len(c.head); room > 0 {
		take := len(p)
		if take > room {
			take = room
		}
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	if len(p) == 0 {
		return
	}
	c.tail = append(c.tail, p...)
	// Trim lazily to twice the tail budget so a stream does not shift bytes on
	// every write.
	if len(c.tail) > 2*c.tailMax {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-c.tailMax:]...)
	}
}

// Total is how many bytes passed through the capture.
func (c *bodyCapture) Total() int64 {
	return c.total
}

// String renders head, the elision marker and tail, in that order.
func (c *bodyCapture) String() string {
	tail := c.tail
	if len(tail) > c.tailMax {
		tail = tail[len(tail)-c.tailMax:]
	}
	kept := int64(len(c.head) + len(tail))
	if c.total <= kept {
		var out bytes.Buffer
		out.Grow(int(kept))
		out.Write(c.head)
		out.Write(tail)
		return out.String()
	}
	var out bytes.Buffer
	out.Grow(int(kept) + markerReserve)
	out.Write(c.head)
	out.WriteString(elidedMarker(int(c.total - kept)))
	out.Write(tail)
	return out.String()
}
