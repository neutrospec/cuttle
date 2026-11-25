package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http/httptrace"
	"strconv"
	"sync"

	"squid/internal/netx"
)

// outConn is an outbound connection of a plain-HTTP transport, direct or
// upstream. It counts up, the request body bytes after transfer decoding,
// as each socket write accepts them, so up never includes bytes still in
// the transport's buffers (design 0002 D7). After a 101 every byte written
// to it is the client's upgraded stream and counts as up.
//
// The transport writes one request at a time on a connection and returns
// it to the pool only after the previous request was written, so the
// connection belongs to one exchange from GotConn until the next GotConn.
type outConn struct {
	net.Conn
	p *Proxy
	// info is the dial's description: its binding and skipped field
	// (design 0008 D15).
	info netx.Info

	mu    sync.Mutex
	ex    *exchange
	meter upMeter
}

func (p *Proxy) newOutConn(c net.Conn, info netx.Info) *outConn {
	return &outConn{Conn: c, p: p, info: info}
}

// assign hands the connection to the exchange whose request the transport
// is about to write on it.
func (c *outConn) assign(ex *exchange) {
	c.mu.Lock()
	c.ex = ex
	c.meter = upMeter{}
	c.mu.Unlock()
}

// upgrade marks the connection as carrying the client's upgraded stream
// after a 101 (design 0002 D7).
func (c *outConn) upgrade() {
	c.mu.Lock()
	c.meter.raw = true
	c.mu.Unlock()
}

func (c *outConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	ex := c.ex
	c.mu.Unlock()
	if ex != nil {
		ex.writes.Add(1)
		defer ex.writes.Add(-1)
	}
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.mu.Lock()
		body := c.meter.feed(b[:n])
		c.mu.Unlock()
		if ex != nil {
			c.p.addBytes(ex, "up", body)
		}
	}
	return n, err
}

// withOutConnTrace returns ctx with a client trace that assigns each
// outbound connection the transport takes, new or pooled, to ex.
func withOutConnTrace(ctx context.Context, ex *exchange) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if oc, ok := info.Conn.(*outConn); ok {
				// The interface is known before any byte is counted.
				oc.p.useConn(ex, oc.Conn, oc.info, !info.Reused)
				oc.assign(ex)
				ex.out.Store(oc)
			}
		},
	})
}

// upMeter finds the request body bytes in the byte stream that Go's
// transport writes on an outbound connection: it skips each request head
// and, for a chunked body, the chunk sizes, the line ends, and the
// trailer. The transport writes these itself, Content-Length and
// Transfer-Encoding included (net/http excludes the client's own), so the
// stream is well formed; the meter reads it only to count, never to
// forward.
type upMeter struct {
	state   meterState
	raw     bool   // after a 101: every byte counts
	line    []byte // the start of the current head or chunk-size line
	length  int64  // Content-Length of the request; -1 when chunked
	remain  int64  // body or chunk bytes still to come
	chunked bool
}

type meterState int

const (
	meterHead      meterState = iota // request line and header fields
	meterBody                        // Content-Length body
	meterChunkSize                   // chunk-size line
	meterChunkData                   // chunk data
	meterChunkEnd                    // the line end after chunk data
	meterTrailer                     // trailer fields after the last chunk
)

// maxMeterLine is how much of each line the meter keeps: enough for a
// Content-Length or Transfer-Encoding field and a chunk size.
const maxMeterLine = 64

// feed consumes bytes in the order written and returns how many of them
// are body bytes.
func (m *upMeter) feed(b []byte) int {
	if m.raw {
		return len(b)
	}
	body := 0
	for len(b) > 0 {
		switch m.state {
		case meterBody, meterChunkData:
			k := int64(len(b))
			if k > m.remain {
				k = m.remain
			}
			body += int(k)
			b = b[k:]
			if m.remain -= k; m.remain == 0 {
				if m.state == meterBody {
					m.state = meterHead
				} else {
					m.state = meterChunkEnd
				}
			}
		default:
			i := bytes.IndexByte(b, '\n')
			part := b
			if i >= 0 {
				part = b[:i]
			}
			if room := maxMeterLine - len(m.line); room > 0 {
				m.line = append(m.line, part[:min(room, len(part))]...)
			}
			if i < 0 {
				return body
			}
			b = b[i+1:]
			m.endLine(bytes.TrimSuffix(m.line, []byte("\r")))
			m.line = m.line[:0]
		}
	}
	return body
}

// endLine handles one complete line in a line-oriented state.
func (m *upMeter) endLine(line []byte) {
	switch m.state {
	case meterHead:
		if len(line) > 0 {
			name, value, ok := bytes.Cut(line, []byte(":"))
			if !ok {
				return // the request line
			}
			value = bytes.TrimSpace(value)
			switch {
			case bytes.EqualFold(bytes.TrimSpace(name), []byte("Content-Length")):
				if n, err := strconv.ParseInt(string(value), 10, 64); err == nil && n >= 0 {
					m.length = n
				}
			case bytes.EqualFold(bytes.TrimSpace(name), []byte("Transfer-Encoding")):
				m.chunked = bytes.Contains(bytes.ToLower(value), []byte("chunked"))
			}
			return
		}
		// The empty line ends the head.
		switch {
		case m.chunked:
			m.state = meterChunkSize
		case m.length > 0:
			m.state, m.remain = meterBody, m.length
		}
		m.length, m.chunked = 0, false
	case meterChunkSize:
		size, _, _ := bytes.Cut(line, []byte(";"))
		n, err := strconv.ParseInt(string(bytes.TrimSpace(size)), 16, 64)
		switch {
		case err != nil || n < 0:
			m.state = meterHead // not expected from Go's writer; resynchronize
		case n == 0:
			m.state = meterTrailer
		default:
			m.state, m.remain = meterChunkData, n
		}
	case meterChunkEnd:
		m.state = meterChunkSize
	case meterTrailer:
		if len(line) == 0 {
			m.state = meterHead
		}
	}
}
