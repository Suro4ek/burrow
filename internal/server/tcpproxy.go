package server

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/suro4ek/burrow/internal/netutil"
)

// serveTCP accepts public connections for a TCP tunnel and pipes each one
// through a fresh stream to the agent. It returns when the listener closes,
// which Registry.Release does on teardown.
func (srv *Server) serveTCP(t *Tunnel, ln net.Listener) {
	log := srv.log.With("tunnel", t.ID, "port", t.Port)
	defer log.Debug("tcp listener stopped")

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient accept error (fd exhaustion, for one) should not
			// kill the tunnel; back off briefly and keep serving.
			log.Warn("accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go srv.pipeTCP(t, conn)
	}
}

// pipeTCP joins one public connection to the agent.
func (srv *Server) pipeTCP(t *Tunnel, conn net.Conn) {
	defer conn.Close()

	ctx, cancel := context.WithTimeout(withRemoteAddr(context.Background(), conn.RemoteAddr().String()), dialTimeout)
	defer cancel()

	upstream, err := t.dial(ctx)
	if err != nil {
		srv.log.Warn("tcp dial failed",
			"tunnel", t.ID, "port", t.Port, "peer", conn.RemoteAddr(), "err", err)
		return
	}
	defer upstream.Close()

	netutil.Join(conn, upstream)
}
