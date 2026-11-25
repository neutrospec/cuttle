package proxy

import (
	"bufio"
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"squid/internal/stats"
	"squid/internal/testenv"
)

// bytesOf returns bytes_total for an action and direction.
func (e *env) bytesOf(action, dir string) int64 {
	return e.p.Live().Get(stats.BytesTotal, "action", action, "direction", dir)
}

// TestTunnelByteCounters checks C12: a controlled transfer of 1,048,576
// bytes down and 65,536 bytes up through a tunnel shows partial bytes_total
// progress while the tunnel is open and exact totals after it closes, and
// tunnels_active returns to 0.
func TestTunnelByteCounters(t *testing.T) {
	e := newEnv(t, baseConfig)
	const up, down, half = 65536, 1 << 20, 1 << 19

	// A raw TCP destination: it reads the whole upload, then sends half of
	// the download, waits for the test, and sends the rest.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	gotUp, release, served := make(chan []byte, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			served <- err
			return
		}
		defer c.Close()
		buf := make([]byte, up)
		if _, err := io.ReadFull(c, buf); err != nil {
			served <- err
			return
		}
		gotUp <- buf
		payload := testenv.Pattern(down)
		if _, err := c.Write(payload[:half]); err != nil {
			served <- err
			return
		}
		<-release
		_, err = c.Write(payload[half:])
		served <- err
	}()
	e.net.Map("bulk.cuttle.test", "203.0.113.20")
	e.net.Route("203.0.113.20:9000", ln.Addr().(*net.TCPAddr).AddrPort())

	c := e.connectRaw("bulk.cuttle.test:9000")
	c.SetDeadline(time.Now().Add(30 * time.Second))
	upload := make([]byte, up)
	crand.Read(upload)
	if _, err := c.Write(upload); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-gotUp:
		if !bytes.Equal(got, upload) {
			t.Fatal("upload changed in transit")
		}
	case err := <-served:
		t.Fatalf("destination: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not arrive")
	}
	first := make([]byte, half)
	if _, err := io.ReadFull(c, first); err != nil {
		t.Fatal(err)
	}

	// Partial progress while the tunnel is open: the upload and exactly the
	// half that the client read, counted before the tunnel ends.
	waitFor(t, "partial counts", func() bool {
		return e.bytesOf("direct", "up") == up && e.bytesOf("direct", "down") == half
	})
	time.Sleep(50 * time.Millisecond)
	if got := e.bytesOf("direct", "down"); got != half {
		t.Errorf("down %d while the destination has sent %d", got, half)
	}
	if got := e.p.Live().Get(stats.TunnelsActive); got != 1 {
		t.Errorf("tunnels_active %d while open", got)
	}
	if ts := e.p.Tunnels(); len(ts) != 1 || ts[0].Target != "bulk.cuttle.test:9000" || ts[0].Kind != "connect" ||
		ts[0].Up != up || ts[0].Down != half || ts[0].Source != "local" || ts[0].Action != "direct" {
		t.Errorf("tunnel list %+v", ts)
	}
	if d := e.logs.decisions(); len(d) != 0 {
		t.Errorf("decision logged while open: %v", d)
	}

	close(release)
	rest, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(append(first, rest...), testenv.Pattern(down)) {
		t.Fatalf("download: %d bytes, %v", half+len(rest), err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	c.Close()

	d := e.waitDecisions(1)[0]
	checkDecision(t, d, map[string]string{"target": "bulk.cuttle.test:9000", "outcome": "ok",
		"up": strconv.Itoa(up), "down": strconv.Itoa(down)})
	if got := e.bytesOf("direct", "up"); got != up {
		t.Errorf("bytes_total up %d, want %d", got, up)
	}
	if got := e.bytesOf("direct", "down"); got != down {
		t.Errorf("bytes_total down %d, want %d", got, down)
	}
	waitFor(t, "tunnels_active 0", func() bool { return e.p.Live().Get(stats.TunnelsActive) == 0 })
	if ts := e.p.Tunnels(); len(ts) != 0 {
		t.Errorf("tunnel list after close %+v", ts)
	}
}

// TestHTTPByteCounters checks C13: plain-HTTP up and down equal the request
// and response body sizes for Content-Length and chunked bodies; up counts
// only body bytes that a socket write to the next hop accepted; after a
// 101 the bytes of an absolute-form WebSocket exchange of known size are
// added to the exchange's counts exactly; a blocked request adds zero
// bytes.
func TestHTTPByteCounters(t *testing.T) {
	e := newEnv(t, baseConfig)
	client, _ := e.goClient()
	// delta runs fn and returns the change of bytes_total{action=direct} and
	// the decision line fn produced.
	delta := func(t *testing.T, fn func()) (up, down int64, d map[string]string) {
		t.Helper()
		n := len(e.logs.decisions())
		u0, d0 := e.bytesOf("direct", "up"), e.bytesOf("direct", "down")
		fn()
		ds := e.waitDecisions(n + 1)
		return e.bytesOf("direct", "up") - u0, e.bytesOf("direct", "down") - d0, ds[n]
	}
	exact := func(t *testing.T, gotUp, gotDown int64, d map[string]string, wantUp, wantDown int) {
		t.Helper()
		if gotUp != int64(wantUp) || gotDown != int64(wantDown) {
			t.Errorf("bytes_total up %d down %d, want %d and %d", gotUp, gotDown, wantUp, wantDown)
		}
		checkDecision(t, d, map[string]string{"up": strconv.Itoa(wantUp), "down": strconv.Itoa(wantDown)})
	}

	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("POST chunked=%v", chunked), func(t *testing.T) {
			body := testenv.Pattern(100_003)
			var resBody []byte
			u, d, line := delta(t, func() {
				var rd io.Reader = bytes.NewReader(body)
				if chunked {
					rd = io.MultiReader(rd) // unknown length: sent chunked
				}
				res, err := client.Post("http://plain.cuttle.test/sink", "application/octet-stream", rd)
				if err != nil {
					t.Fatal(err)
				}
				resBody, _ = io.ReadAll(res.Body)
				res.Body.Close()
			})
			if string(resBody) != strconv.Itoa(len(body)) {
				t.Fatalf("origin counted %q", resBody)
			}
			exact(t, u, d, line, len(body), len(resBody))
		})
	}

	t.Run("GET Content-Length and chunked responses", func(t *testing.T) {
		for _, c := range []struct {
			path string
			size int
		}{{"/bytes?n=123457", 123457}, {"/chunked", len("chunk-0;chunk-1;chunk-2;")}} {
			u, d, line := delta(t, func() {
				res, err := client.Get("http://plain.cuttle.test" + c.path)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
			})
			exact(t, u, d, line, 0, c.size)
		}
	})

	t.Run("upload cut by the next hop", func(t *testing.T) {
		// The next hop's socket accepts only the first 20,001 bytes. up
		// counts the body bytes among them, not what the transport read
		// from the client beyond them.
		const limit = 20_001
		e.net.Map("cut.cuttle.test", "203.0.113.30")
		e.net.Route("203.0.113.30:80", e.http.Addr)
		e.net.LimitWrites("203.0.113.30:80", limit)
		e.http.Reset()
		body := testenv.Pattern(200_000)
		u, d, line := delta(t, func() {
			c := e.dial()
			go func() {
				fmt.Fprintf(c, "POST http://cut.cuttle.test/sink HTTP/1.1\r\nHost: cut.cuttle.test\r\nContent-Length: %d\r\n\r\n", len(body))
				c.Write(body)
			}()
			res, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusBadGateway {
				t.Errorf("status %d %q", res.StatusCode, res.Header.Get("Proxy-Status"))
			}
		})
		var raw []byte
		waitFor(t, "origin read", func() bool {
			reqs := e.http.Requests()
			if len(reqs) == 0 {
				return false
			}
			raw = e.http.Raw(reqs[0].Conn)
			return len(raw) == limit
		})
		head := bytes.Index(raw, []byte("\r\n\r\n")) + 4
		if head < 4 {
			t.Fatalf("no head in %q", raw)
		}
		want := limit - head
		if u != int64(want) || line["up"] != strconv.Itoa(want) {
			t.Errorf("up %d (decision %s), want %d socket-accepted body bytes", u, line["up"], want)
		}
		if d != 0 {
			t.Errorf("down %d", d)
		}
	})

	t.Run("WebSocket after 101", func(t *testing.T) {
		var sent, read int
		u, d, line := delta(t, func() {
			c := e.dial()
			io.WriteString(c, "GET http://ws.cuttle.test/ws HTTP/1.1\r\nHost: ws.cuttle.test\r\nUpgrade: websocket\r\n"+
				"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"+
				"Origin: http://ws.cuttle.test\r\n\r\n")
			br := bufio.NewReader(c)
			head, err := readHead(br)
			if err != nil || !strings.HasPrefix(head, "HTTP/1.1 101 ") {
				t.Fatalf("upgrade: %q %v", head, err)
			}
			for _, size := range []int{5, 125, 126, 1000, 70000} {
				payload := testenv.Pattern(size)
				frame := wsClientFrame(0x1, payload)
				if _, err := c.Write(frame); err != nil {
					t.Fatal(err)
				}
				sent += len(frame)
				echo, n, err := readWSFrame(br)
				if err != nil || !bytes.Equal(echo, payload) {
					t.Fatalf("echo of %d bytes: %d bytes, %v", size, len(echo), err)
				}
				read += n
			}
			if br.Buffered() != 0 {
				t.Fatalf("%d unexpected bytes", br.Buffered())
			}
			// The client ends its side; cuttle then ends the exchange.
			c.(interface{ CloseWrite() error }).CloseWrite()
			rest, _ := io.ReadAll(br)
			read += len(rest)
		})
		checkDecision(t, line, map[string]string{"kind": "http", "status": "101", "outcome": "ok"})
		exact(t, u, d, line, sent, read)
	})

	t.Run("blocked request", func(t *testing.T) {
		before := e.bytesTotal()
		res, _ := e.raw("POST http://blocked.cuttle.test/sink HTTP/1.1\r\nHost: blocked.cuttle.test\r\nContent-Length: 5\r\n\r\nhello")
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d", res.StatusCode)
		}
		ds := e.logs.decisions()
		checkDecision(t, ds[len(ds)-1], map[string]string{"outcome": "blocked", "up": "0", "down": "0"})
		if after := e.bytesTotal(); after != before {
			t.Errorf("blocked request added %d bytes", after-before)
		}
	})
}

// readHead reads a message head up to and including its empty line.
func readHead(br *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		b.WriteString(line)
		if err != nil {
			return b.String(), err
		}
		if line == "\r\n" {
			return b.String(), nil
		}
	}
}

// wsClientFrame builds a final, masked WebSocket frame (RFC 6455 section
// 5.2) as a client sends it.
func wsClientFrame(opcode byte, payload []byte) []byte {
	f := []byte{0x80 | opcode}
	switch n := len(payload); {
	case n < 126:
		f = append(f, 0x80|byte(n))
	case n <= 0xffff:
		f = append(f, 0x80|126)
		f = binary.BigEndian.AppendUint16(f, uint16(n))
	default:
		f = append(f, 0x80|127)
		f = binary.BigEndian.AppendUint64(f, uint64(n))
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	f = append(f, mask...)
	for i, b := range payload {
		f = append(f, b^mask[i%4])
	}
	return f
}

// readWSFrame reads one unmasked server frame and returns its payload and
// the frame's size on the wire.
func readWSFrame(br *bufio.Reader) ([]byte, int, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return nil, 0, err
	}
	size, wire := int(h[1]&0x7f), 2
	switch size {
	case 126:
		var l [2]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return nil, 0, err
		}
		size, wire = int(binary.BigEndian.Uint16(l[:])), wire+2
	case 127:
		var l [8]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return nil, 0, err
		}
		size, wire = int(binary.BigEndian.Uint64(l[:])), wire+8
	}
	payload := make([]byte, size)
	_, err := io.ReadFull(br, payload)
	return payload, wire + size, err
}

// TestUpMeter checks the outbound byte meter on requests as Go writes them:
// over a stream of a request without a body, a Content-Length body, a
// chunked body with a trailer, and an empty body, every prefix counts
// exactly the body bytes it holds, however the writes are split; after a
// 101 every byte counts (design 0002 D7).
func TestUpMeter(t *testing.T) {
	var stream bytes.Buffer
	get, _ := http.NewRequest("GET", "http://h/a?b", nil)
	get.Write(&stream)
	cl, _ := http.NewRequest("POST", "http://h/cl", bytes.NewReader(testenv.Pattern(1000)))
	cl.Header.Set("X-Note", "Content-Length: 7")
	cl.Write(&stream)
	ch, _ := http.NewRequest("POST", "http://h/ch", &pieces{data: testenv.Pattern(1234), size: 300})
	ch.ContentLength = -1
	ch.Trailer = http.Header{"X-Sum": {"1"}}
	ch.Write(&stream)
	empty, _ := http.NewRequest("POST", "http://h/empty", http.NoBody)
	empty.Write(&stream)
	wire := stream.Bytes()
	if !bytes.Contains(wire, []byte("Transfer-Encoding: chunked")) || !bytes.Contains(wire, []byte("12c\r\n")) {
		t.Fatalf("unexpected framing:\n%s", wire)
	}
	mask := bodyMask(t, wire)
	total := 0
	for _, b := range mask {
		if b {
			total++
		}
	}
	if total != 1000+1234 {
		t.Fatalf("reference mask counts %d body bytes", total)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for cut := 0; cut <= len(wire); cut++ {
		var m upMeter
		got := 0
		for rest := wire[:cut]; len(rest) > 0; {
			k := 1 + rng.IntN(min(len(rest), 700))
			got += m.feed(rest[:k])
			rest = rest[k:]
		}
		want := 0
		for _, b := range mask[:cut] {
			if b {
				want++
			}
		}
		if got != want {
			t.Fatalf("prefix of %d bytes: counted %d, want %d", cut, got, want)
		}
	}

	var m upMeter
	if n := m.feed(wire); n != total {
		t.Fatalf("whole stream %d", n)
	}
	m.raw = true
	if n := m.feed([]byte("\x81\x05hello")); n != 7 {
		t.Errorf("raw mode counted %d", n)
	}
}

// pieces returns its data in reads of at most size bytes, so Go writes one
// chunk per read.
type pieces struct {
	data []byte
	size int
}

func (p *pieces) Read(b []byte) (int, error) {
	if len(p.data) == 0 {
		return 0, io.EOF
	}
	n := copy(b[:min(len(b), p.size)], p.data)
	p.data = p.data[n:]
	return n, nil
}

// bodyMask marks the body bytes of a stream of requests, decoding it whole
// as a reference for the incremental meter.
func bodyMask(t *testing.T, wire []byte) []bool {
	t.Helper()
	mask := make([]bool, len(wire))
	i := 0
	line := func() string {
		j := bytes.Index(wire[i:], []byte("\r\n"))
		if j < 0 {
			t.Fatalf("no line end at %d", i)
		}
		l := string(wire[i : i+j])
		i += j + 2
		return l
	}
	for i < len(wire) {
		length, chunked := 0, false
		line() // request line
		for l := line(); l != ""; l = line() {
			name, value, _ := strings.Cut(l, ":")
			switch strings.TrimSpace(name) {
			case "Content-Length":
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			case "Transfer-Encoding":
				chunked = strings.TrimSpace(value) == "chunked"
			}
		}
		if !chunked {
			for k := 0; k < length; k++ {
				mask[i+k] = true
			}
			i += length
			continue
		}
		for {
			size, err := strconv.ParseInt(line(), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			if size == 0 {
				break
			}
			for k := 0; k < int(size); k++ {
				mask[i+k] = true
			}
			i += int(size)
			if line() != "" {
				t.Fatal("chunk not followed by CRLF")
			}
		}
		for line() != "" { // trailer
		}
	}
	return mask
}

// TestUpgradedConnections checks OI-0006 R5: a connection upgraded after a
// 101 is tracked like a tunnel. Status lists it, the idle limit closes it,
// and Shutdown closes it with outcome shutdown (design 0002 D5, 0006 D6).
// An exchange still in flight when the drain ends also logs shutdown.
// upgrade opens an absolute-form WebSocket through the instance and
// completes one echo of "ping".
func upgrade(t *testing.T, e *env) (net.Conn, *bufio.Reader) {
	t.Helper()
	c := e.dial()
	io.WriteString(c, "GET http://ws.cuttle.test/ws HTTP/1.1\r\nHost: ws.cuttle.test\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"+
		"Origin: http://ws.cuttle.test\r\n\r\n")
	br := bufio.NewReader(c)
	if head, err := readHead(br); err != nil || !strings.HasPrefix(head, "HTTP/1.1 101 ") {
		t.Fatalf("upgrade: %q %v", head, err)
	}
	c.Write(wsClientFrame(0x1, []byte("ping")))
	if echo, _, err := readWSFrame(br); err != nil || string(echo) != "ping" {
		t.Fatalf("echo %q %v", echo, err)
	}
	return c, br
}

func TestUpgradedConnections(t *testing.T) {
	closedWithin := func(t *testing.T, br *bufio.Reader, max time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		done := make(chan error, 1)
		go func() { _, err := br.ReadByte(); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("read a byte instead of a close")
			}
		case <-time.After(max):
			t.Fatalf("upgraded connection still open after %v", max)
		}
		return time.Since(start)
	}

	t.Run("idle limit", func(t *testing.T) {
		cfg := strings.Replace(baseConfig, `default = "direct"`, "default = \"direct\"\nidle_timeout = \"300ms\"", 1)
		e := newEnv(t, cfg)
		_, br := upgrade(t, e)
		if el := closedWithin(t, br, 5*time.Second); el < 200*time.Millisecond {
			t.Errorf("closed after %v, before the idle limit", el)
		}
		checkDecision(t, e.waitDecisions(1)[0], map[string]string{"status": "101", "outcome": "ok"})
		waitFor(t, "untracked", func() bool { return len(e.p.Tunnels()) == 0 })
	})

	t.Run("shutdown", func(t *testing.T) {
		e := newEnv(t, baseConfig)
		_, br := upgrade(t, e)
		// The echo of "ping" is a 6-byte frame.
		waitFor(t, "upgraded connection listed", func() bool {
			ts := e.p.Tunnels()
			return len(ts) == 1 && ts[0].Kind == "upgrade" && ts[0].Target == "ws.cuttle.test:80" && ts[0].Down == 6
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.p.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		closedWithin(t, br, 5*time.Second)
		checkDecision(t, e.waitDecisions(1)[0], map[string]string{"status": "101", "outcome": "shutdown"})
	})

	t.Run("exchange beyond the drain", func(t *testing.T) {
		e := newEnv(t, baseConfig)
		c := e.dial()
		io.WriteString(c, "GET http://plain.cuttle.test/slow?ms=5000 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
		waitFor(t, "request in flight", func() bool { return e.p.Live().Get(stats.HTTPRequestsActive) == 1 })
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		e.p.Shutdown(ctx)
		checkDecision(t, e.waitDecisions(1)[0], map[string]string{"kind": "http", "outcome": "shutdown"})
	})
}

// TestShutdownWaitsForHandlers checks the order of design 0006 D6 (L-R1 of
// the TASK-0007 review): when Shutdown returns, every tunnel, upgraded
// connection, and exchange it closed has written its decision line with
// outcome shutdown, so the caller's stopped line comes last.
func TestShutdownWaitsForHandlers(t *testing.T) {
	e := newEnv(t, baseConfig)
	const tunnels, upgrades, slow = 50, 5, 5
	for range tunnels {
		e.connectRaw("plain.cuttle.test:80")
	}
	for range upgrades {
		upgrade(t, e)
	}
	for range slow {
		c := e.dial()
		io.WriteString(c, "GET http://plain.cuttle.test/slow?ms=5000 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
	}
	waitFor(t, "every connection open", func() bool {
		return len(e.p.Tunnels()) == tunnels+upgrades && e.p.Live().Get(stats.HTTPRequestsActive) == upgrades+slow
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	e.p.Shutdown(ctx)
	el := time.Since(start)
	n := 0
	for _, d := range e.logs.decisions() {
		if d["outcome"] == OutcomeShutdown {
			n++
		}
	}
	if n != tunnels+upgrades+slow {
		t.Errorf("%d shutdown decision lines when Shutdown returned, want %d", n, tunnels+upgrades+slow)
	}
	if el > 200*time.Millisecond+handlerWait {
		t.Errorf("Shutdown took %v", el)
	}
	if strings.Contains(e.logs.String(), "handlers still running") {
		t.Errorf("handlers outlived the wait:\n%s", e.logs.String())
	}
}
