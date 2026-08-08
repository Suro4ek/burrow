package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/suro4ek/burrow/internal/netutil"
	"github.com/suro4ek/burrow/internal/proto"
)

// handshakeTimeout bounds how long an unauthenticated connection may sit on a
// control listener slot.
const handshakeTimeout = 15 * time.Second

// dialTimeout bounds the round trip that asks an agent to reach its local
// service and report back.
const dialTimeout = 15 * time.Second

// ctxKeyRemoteAddr carries the end user's address down to Tunnel.dial so the
// agent can log who is calling.
type ctxKeyRemoteAddr struct{}

// withRemoteAddr annotates a context with the originating client address.
func withRemoteAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, ctxKeyRemoteAddr{}, addr)
}

// Session is one connected agent.
type Session struct {
	ID         string
	TokenID    string
	TokenName  string
	Hostname   string
	RemoteAddr string
	Started    time.Time

	srv *Server
	mux *yamux.Session

	mu      sync.Mutex
	tunnels []*Tunnel
	closed  bool
}

// Tunnels returns a snapshot of this session's tunnels.
func (s *Session) Tunnels() []*Tunnel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Tunnel(nil), s.tunnels...)
}

// Close drops the agent connection. Tunnel teardown follows from the session
// goroutine noticing the mux is gone, so this is safe to call from anywhere.
func (s *Session) Close() {
	if s.mux != nil {
		_ = s.mux.Close()
	}
}

// handleAgent runs one agent connection to completion.
func (srv *Server) handleAgent(conn net.Conn) {
	defer conn.Close()

	sess := &Session{
		ID:         netutil.RandID(8),
		RemoteAddr: conn.RemoteAddr().String(),
		Started:    time.Now(),
		srv:        srv,
	}
	log := srv.log.With("session", sess.ID, "peer", sess.RemoteAddr)

	ycfg := yamux.DefaultConfig()
	ycfg.EnableKeepAlive = true
	ycfg.KeepAliveInterval = 20 * time.Second
	ycfg.ConnectionWriteTimeout = 20 * time.Second
	ycfg.LogOutput = slogWriter{log: log}

	mux, err := yamux.Server(conn, ycfg)
	if err != nil {
		log.Warn("yamux setup failed", "err", err)
		return
	}
	defer mux.Close()
	sess.mux = mux

	// The agent opens the control stream first thing; anything slower than
	// handshakeTimeout is not a healthy agent.
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	ctl, err := mux.AcceptStream()
	if err != nil {
		log.Debug("no control stream", "err", err)
		return
	}
	defer ctl.Close()

	if err := sess.handshake(ctl); err != nil {
		log.Warn("handshake rejected", "err", err)
		return
	}
	// Past the handshake the connection is long-lived; yamux keepalives, not
	// socket deadlines, detect a dead peer from here on.
	_ = conn.SetDeadline(time.Time{})

	log = log.With("token", sess.TokenName, "hostname", sess.Hostname)
	log.Info("agent connected")
	srv.addSession(sess)
	defer func() {
		srv.removeSession(sess)
		sess.closeAll()
		log.Info("agent disconnected", "uptime", time.Since(sess.Started).Round(time.Second))
	}()

	sess.serveControl(ctl, log)
}

// handshake authenticates the agent. It writes a HelloResp either way so the
// agent can print a real reason instead of "connection closed".
func (s *Session) handshake(ctl net.Conn) error {
	var hello proto.Hello
	if err := proto.ReadAs(ctl, proto.TypeHello, &hello); err != nil {
		return err
	}
	reject := func(reason string) error {
		_ = proto.Write(ctl, proto.TypeHelloResp, proto.HelloResp{OK: false, Error: reason})
		return errors.New(reason)
	}
	if hello.Version != proto.Version {
		return reject(fmt.Sprintf("protocol version mismatch: agent speaks %d, server speaks %d",
			hello.Version, proto.Version))
	}
	tok, ok := s.srv.store.Lookup(hello.Token)
	if !ok {
		return reject("invalid token")
	}
	if tok.Disabled {
		return reject("this token is disabled")
	}
	s.TokenID, s.TokenName = tok.ID, tok.Name
	s.Hostname = hello.Hostname
	s.srv.store.TouchLastSeen(tok.ID)

	return proto.Write(ctl, proto.TypeHelloResp, proto.HelloResp{
		OK:         true,
		Server:     s.srv.version,
		BaseDomain: s.srv.cfg.BaseDomain,
	})
}

// serveControl processes control messages until the agent goes away.
func (s *Session) serveControl(ctl net.Conn, log *slog.Logger) {
	for {
		env, err := proto.Read(ctl)
		if err != nil {
			return // agent closed, or the session died
		}
		switch env.Type {
		case proto.TypeTunnelReq:
			var req proto.TunnelReq
			if err := json.Unmarshal(env.Data, &req); err != nil {
				_ = proto.Write(ctl, proto.TypeTunnelResp, proto.TunnelResp{
					OK: false, Error: "malformed tunnel request",
				})
				continue
			}
			resp := s.openTunnel(req, log)
			if err := proto.Write(ctl, proto.TypeTunnelResp, resp); err != nil {
				return
			}
		default:
			log.Debug("ignoring unexpected control message", "type", env.Type)
		}
	}
}

// openTunnel validates a request against the session's token and publishes it.
func (s *Session) openTunnel(req proto.TunnelReq, log *slog.Logger) proto.TunnelResp {
	fail := func(format string, args ...any) proto.TunnelResp {
		return proto.TunnelResp{OK: false, Error: fmt.Sprintf(format, args...)}
	}
	cfg := s.srv.cfg

	// Re-read the token instead of trusting the handshake snapshot: an admin
	// may have changed limits or reservations while this agent was connected.
	tok, ok := s.srv.store.Get(s.TokenID)
	if !ok {
		return fail("token no longer exists")
	}
	if tok.Disabled {
		return fail("this token is disabled")
	}

	s.mu.Lock()
	n := len(s.tunnels)
	s.mu.Unlock()
	limit := cfg.MaxTunnelsPerSession
	if tok.MaxTunnels > 0 && tok.MaxTunnels < limit {
		limit = tok.MaxTunnels
	}
	if n >= limit {
		return fail("tunnel limit reached (%d)", limit)
	}

	t := &Tunnel{
		ID:        netutil.RandID(12),
		Proto:     req.Proto,
		LocalAddr: req.LocalAddr,
		Created:   time.Now(),
		sess:      s,
	}

	switch req.Proto {
	case proto.ProtoHTTP:
		sub := normalizeSubdomain(req.Subdomain)
		if sub != "" {
			if !validSubdomain(sub) {
				return fail("invalid subdomain %q", req.Subdomain)
			}
			if reservedSubdomains[sub] {
				return fail("subdomain %q is reserved by the server", sub)
			}
			if err := s.srv.store.MayUseSubdomain(tok.ID, sub, cfg.FreeSubdomains); err != nil {
				return fail("%s", err)
			}
		}
		t.proxy = s.srv.newProxy(t)
		if err := s.srv.reg.ClaimHTTP(t, sub); err != nil {
			return fail("%s", err)
		}

	case proto.ProtoTCP:
		if tok.DenyTCP {
			return fail("TCP tunnels are not allowed for this token")
		}
		port := req.RemotePort
		if port == 0 {
			// A token with reserved ports should get a stable address without
			// having to ask for it, so `burrow ssh` keeps the same port across
			// restarts. Fall through to random allocation if all are busy.
			port = s.srv.reg.FirstFreePort(s.srv.store.ReservedPorts(tok.ID))
		} else if err := s.srv.store.MayUsePort(tok.ID, port, cfg.FreePorts); err != nil {
			return fail("%s", err)
		}
		ln, err := s.srv.reg.ClaimTCP(t, port)
		if err != nil {
			return fail("%s", err)
		}
		go s.srv.serveTCP(t, ln)

	default:
		return fail("unknown tunnel protocol %q", req.Proto)
	}

	s.mu.Lock()
	if s.closed {
		// The session died while we were allocating; do not leak the claim.
		s.mu.Unlock()
		s.srv.reg.Release(t)
		return fail("session is closing")
	}
	s.tunnels = append(s.tunnels, t)
	s.mu.Unlock()

	log.Info("tunnel opened",
		"id", t.ID, "proto", t.Proto, "public", t.Public(cfg), "local", t.LocalAddr)

	return proto.TunnelResp{
		OK:         true,
		ID:         t.ID,
		Proto:      t.Proto,
		URL:        t.Public(cfg),
		RemoteHost: cfg.PublicHost,
		RemotePort: t.Port,
	}
}

// closeAll tears down every tunnel this session published.
func (s *Session) closeAll() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	tunnels := s.tunnels
	s.tunnels = nil
	s.mu.Unlock()

	for _, t := range tunnels {
		s.srv.reg.Release(t)
	}
}

// dial opens a data stream to the agent for one incoming user connection and
// waits for the agent to confirm it reached the local service.
func (t *Tunnel) dial(ctx context.Context) (net.Conn, error) {
	stream, err := t.sess.mux.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	deadline := time.Now().Add(dialTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = stream.SetDeadline(deadline)

	remote, _ := ctx.Value(ctxKeyRemoteAddr{}).(string)
	if err := proto.Write(stream, proto.TypeStreamOpen, proto.StreamOpen{
		TunnelID:   t.ID,
		RemoteAddr: remote,
	}); err != nil {
		_ = stream.Close()
		return nil, err
	}

	var ack proto.StreamAck
	if err := proto.ReadAs(stream, proto.TypeStreamAck, &ack); err != nil {
		_ = stream.Close()
		return nil, err
	}
	if !ack.OK {
		_ = stream.Close()
		return nil, fmt.Errorf("agent could not reach %s: %s", t.LocalAddr, ack.Error)
	}

	// Hand back a stream with no deadline: what follows may be a websocket or
	// an SSH session that idles for hours.
	_ = stream.SetDeadline(time.Time{})

	t.Conns.Add(1)
	// Counting here covers HTTP and raw TCP alike, since both protocols reach
	// the agent through this one stream.
	return netutil.Count(stream, &t.Traffic), nil
}

// slogWriter adapts yamux's io.Writer logging to slog at debug level.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.log.Debug("yamux: " + string(bytesTrimNewline(p)))
	return len(p), nil
}

func bytesTrimNewline(p []byte) []byte {
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	return p
}
