package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"squid/internal/stats"
)

// connectEstablished is the exact response to a successful CONNECT; Chrome
// accepts only an exact 200 (design 0002 D5).
const connectEstablished = "HTTP/1.1 200 Connection established\r\n\r\n"

// connectDirect dials the target, writes the exact 200, and relays bytes,
// starting with any bytes the client pipelined after its head.
func (p *Proxy) connectDirect(w http.ResponseWriter, r *http.Request, ex *exchange) {
	server, err := ex.snap.dialer.Dial(r.Context(), ex.d)
	if err != nil {
		p.dialFailure(w, ex, err)
		return
	}
	p.establish(w, ex, server, nil)
}

// establish takes the client connection, writes the exact 200, and relays
// bytes between the client and server, the connected next hop. Bytes the
// client pipelined after its head go to the server first, and early bytes
// that the server sent after its own answer go to the client first (design
// 0003 D3).
func (p *Proxy) establish(w http.ResponseWriter, ex *exchange, server net.Conn, early []byte) {
	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		server.Close()
		p.reply(w, ex, http.StatusInternalServerError, "proxy_internal_error", "hijack failed", "internal error", OutcomeInternal)
		return
	}
	var pipelined []byte
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		pipelined = bytes.Clone(b)
	}
	if _, err := io.WriteString(client, connectEstablished); err != nil {
		client.Close()
		server.Close()
		ex.outcome = OutcomeClientClosed
		p.finish(ex)
		return
	}
	ex.status = http.StatusOK

	t := &tunnel{client: client, server: server, ex: ex}
	if !p.track(t) {
		t.closeBoth()
		ex.outcome = OutcomeShutdown
		p.finish(ex)
		return
	}
	p.live.Add(1, stats.TunnelsActive)
	t.relay(pipelined, early, ex.snap.cfg.Proxy.IdleTimeout, func(dir string, n int) { p.addBytes(ex, dir, n) })
	p.live.Add(-1, stats.TunnelsActive)
	p.untrack(t)
	ex.outcome = OutcomeOK
	if t.wasShutdown.Load() {
		ex.outcome = OutcomeShutdown
	}
	p.finish(ex)
}

// tracked is a client connection that net/http no longer tracks once the
// handler took it: a CONNECT tunnel, or a connection upgraded after a 101.
// Shutdown closes each one, which then logs outcome shutdown, and status
// lists them (design 0002 D7, 0006 D6).
type tracked interface {
	shutdown()
	status() TunnelStatus
}

// track registers a connection so Shutdown can close it. It returns false
// once shutdown has started.
func (p *Proxy) track(t tracked) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	p.tunnels[t] = struct{}{}
	return true
}

func (p *Proxy) untrack(t tracked) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tunnels, t)
}

// trackedStatus describes the exchange of a tracked connection.
func trackedStatus(ex *exchange, kind string) TunnelStatus {
	return TunnelStatus{
		Target:   ex.target,
		Kind:     kind,
		Action:   ex.d.Action.String(),
		Upstream: ex.d.Upstream,
		Source:   ex.source.String(),
		Started:  ex.start,
		Up:       ex.up.Load(),
		Down:     ex.down.Load(),
	}
}

// watchIdle calls closeFn once no activity was recorded in last, in unix
// nanoseconds, for idle; 0 disables it. The returned function stops the
// watch.
func watchIdle(last *atomic.Int64, idle time.Duration, closeFn func()) (stop func()) {
	if idle <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				since := time.Since(time.Unix(0, last.Load()))
				if since >= idle {
					closeFn()
					return
				}
				timer.Reset(idle - since)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// tunnel relays bytes between a client and its next hop (design 0002 D5).
type tunnel struct {
	client, server net.Conn
	ex             *exchange
	closeOnce      sync.Once
	wasShutdown    atomic.Bool
	last           atomic.Int64 // unix nanoseconds of the last write
}

func (t *tunnel) status() TunnelStatus { return trackedStatus(t.ex, "connect") }

func (t *tunnel) closeBoth() {
	t.closeOnce.Do(func() {
		t.client.Close()
		t.server.Close()
	})
}

func (t *tunnel) shutdown() {
	t.wasShutdown.Store(true)
	t.closeBoth()
}

// relay copies both directions with 32 KiB buffers, after writing the
// client's pipelined bytes to the server and the server's early bytes to
// the client. On EOF in one direction it half-closes the other side's write
// half and keeps copying the other direction. Both connections close when
// both directions finish, on any error, or after idle with no bytes in
// either direction (0 disables the idle limit). count is called as each
// write completes.
func (t *tunnel) relay(pipelined, early []byte, idle time.Duration, count func(dir string, n int)) {
	t.last.Store(time.Now().UnixNano())
	if len(pipelined) > 0 {
		n, err := t.server.Write(pipelined)
		count("up", n)
		if err != nil {
			t.closeBoth()
			return
		}
	}
	if len(early) > 0 {
		n, err := t.client.Write(early)
		count("down", n)
		if err != nil {
			t.closeBoth()
			return
		}
	}
	defer watchIdle(&t.last, idle, t.closeBoth)()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.copy(t.server, t.client, "up", count) }()
	go func() { defer wg.Done(); t.copy(t.client, t.server, "down", count) }()
	wg.Wait()
	t.closeBoth()
}

func (t *tunnel) copy(dst, src net.Conn, dir string, count func(string, int)) {
	buf := make([]byte, relayBufferSize)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			count(dir, w)
			t.last.Store(time.Now().UnixNano())
			if werr != nil {
				t.closeBoth()
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if cw, ok := dst.(interface{ CloseWrite() error }); !ok || cw.CloseWrite() != nil {
					t.closeBoth()
				}
			} else {
				t.closeBoth()
			}
			return
		}
	}
}
