// Package netutil holds helpers shared by the server and the agent.
package netutil

import (
	"crypto/rand"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// idAlphabet omits characters that are easy to misread out loud (0/o, 1/l).
const idAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// RandID returns a cryptographically random, URL- and DNS-safe identifier.
func RandID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on any platform we target; if it ever
		// does, failing loudly beats handing out predictable subdomains.
		panic("netutil: crypto/rand unavailable: " + err.Error())
	}
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}

// closeWriter is implemented by *net.TCPConn and *tls.Conn. yamux streams are
// not: their Close sends a FIN and leaves the read side open, which is exactly
// the half-close semantics we want here.
type closeWriter interface {
	CloseWrite() error
}

// Join copies bytes in both directions until both sides are done, propagating
// EOF as a half-close so that protocols which signal end-of-request by closing
// the write side (HTTP/1.0, some database clients) keep working.
func Join(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// Counter accumulates traffic in both directions of one tunnel.
type Counter struct {
	// In counts bytes travelling from the public side toward the local
	// service; Out counts the replies coming back.
	In, Out atomic.Int64
	// LastActive is a Unix timestamp, updated as data flows.
	LastActive atomic.Int64
}

// CountingConn wraps the connection to the agent and attributes its traffic to
// a Counter. Wrapping here rather than at each call site means HTTP and raw
// TCP tunnels are measured by the same code.
type CountingConn struct {
	net.Conn
	c *Counter
}

// Count wraps conn so that reads and writes accumulate into c.
func Count(conn net.Conn, c *Counter) net.Conn {
	return &CountingConn{Conn: conn, c: c}
}

// Read pulls bytes coming back from the local service.
func (w *CountingConn) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		w.c.Out.Add(int64(n))
		w.c.LastActive.Store(time.Now().Unix())
	}
	return n, err
}

// Write pushes bytes from the public side toward the local service.
func (w *CountingConn) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if n > 0 {
		w.c.In.Add(int64(n))
		w.c.LastActive.Store(time.Now().Unix())
	}
	return n, err
}

// CloseWrite forwards the half-close when the wrapped connection supports it,
// so wrapping does not downgrade Join's shutdown behaviour.
func (w *CountingConn) CloseWrite() error {
	if cw, ok := w.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return w.Conn.Close()
}
