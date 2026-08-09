// Package client implements the agent that runs next to the local service and
// keeps one multiplexed connection open to the tunnel server.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/suro4ek/burrow/internal/netutil"
	"github.com/suro4ek/burrow/internal/proto"
)

// Config configures an agent.
type Config struct {
	// ServerAddr is the tunnel server's control endpoint, host:port.
	ServerAddr string
	// Token authenticates the agent.
	Token string
	// TLS wraps the control connection in TLS. Leave it on unless the server
	// is reachable only over a trusted private network.
	TLS bool
	// TLSServerName overrides the name verified in the server certificate.
	TLSServerName string
	// Insecure skips certificate verification. Only useful with self-signed
	// certificates during setup; it defeats the point of TLS otherwise.
	Insecure bool
	// Tunnels are the endpoints to publish.
	Tunnels []TunnelSpec
	// Version is reported to the server for logging.
	Version string
	// Log receives diagnostics.
	Log *slog.Logger
	// OnReady is called after every successful (re)connect with the tunnels
	// the server actually granted.
	OnReady func(granted []proto.TunnelResp)
	// OnAuthorizedKeys is called on every handshake with the SSH keys the
	// server says may open a session. Called on reconnects too, so revoking a
	// key takes effect without restarting the agent.
	OnAuthorizedKeys func(keys []string)
}

// ErrRejected is returned when the server refuses the agent outright. Retrying
// will not help, so Run stops instead of looping.
var ErrRejected = errors.New("rejected by server")

// localDialTimeout bounds how long the agent waits on the local service before
// telling the server the origin is down.
const localDialTimeout = 10 * time.Second

// Client is a reconnecting tunnel agent.
type Client struct {
	cfg Config
	log *slog.Logger

	mu      sync.RWMutex
	routes  map[string]string // tunnel ID -> local address
	granted []proto.TunnelResp
}

// New returns an agent for cfg.
func New(cfg Config) (*Client, error) {
	if cfg.ServerAddr == "" {
		return nil, errors.New("server address is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("token is required")
	}
	if len(cfg.Tunnels) == 0 {
		return nil, errors.New("at least one tunnel is required")
	}
	if _, _, err := net.SplitHostPort(cfg.ServerAddr); err != nil {
		return nil, fmt.Errorf("server address %q must be host:port", cfg.ServerAddr)
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, log: log, routes: make(map[string]string)}, nil
}

// Granted returns the tunnels the server most recently confirmed.
func (c *Client) Granted() []proto.TunnelResp {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]proto.TunnelResp(nil), c.granted...)
}

// Run keeps a session up until ctx is cancelled or the server rejects us.
func (c *Client) Run(ctx context.Context) error {
	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
		// A session that lasted this long counts as healthy, so the next
		// failure starts backing off from scratch.
		stableAfter = 30 * time.Second
	)

	backoff := minBackoff
	for {
		started := time.Now()
		err := c.session(ctx)

		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRejected) {
			return err
		}
		if time.Since(started) >= stableAfter {
			backoff = minBackoff
		}
		c.log.Warn("disconnected, retrying", "err", err, "in", backoff)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session runs one connection from dial to disconnect.
func (c *Client) session(ctx context.Context) error {
	conn, err := c.dialServer(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	ycfg := yamux.DefaultConfig()
	ycfg.EnableKeepAlive = true
	ycfg.KeepAliveInterval = 20 * time.Second
	ycfg.ConnectionWriteTimeout = 20 * time.Second
	ycfg.LogOutput = slogWriter{log: c.log}

	mux, err := yamux.Client(conn, ycfg)
	if err != nil {
		return fmt.Errorf("yamux setup: %w", err)
	}
	defer mux.Close()

	ctl, err := mux.OpenStream()
	if err != nil {
		return c.explainHandshake(fmt.Errorf("open control stream: %w", err))
	}
	defer ctl.Close()

	if err := c.handshake(ctl); err != nil {
		return c.explainHandshake(err)
	}
	granted, err := c.register(ctl)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.granted = granted
	c.mu.Unlock()
	if c.cfg.OnReady != nil {
		c.cfg.OnReady(granted)
	}

	// The control stream carries nothing else right now; reading it is how we
	// learn the server hung up.
	ctlDone := make(chan error, 1)
	go func() {
		for {
			env, err := proto.Read(ctl)
			if err != nil {
				ctlDone <- err
				return
			}
			c.log.Debug("control message", "type", env.Type)
		}
	}()

	accepted := make(chan error, 1)
	go func() { accepted <- c.acceptStreams(mux) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-ctlDone:
		return fmt.Errorf("control stream closed: %w", err)
	case err := <-accepted:
		return err
	}
}

// explainHandshake turns a bare connection error into something actionable.
//
// Every way of pointing the agent at the wrong thing collapses into the same
// unhelpful "EOF": a plaintext agent against a TLS control port, or an agent
// aimed at the public HTTPS port instead of the control port. The transport
// cannot tell these apart, so say what they are.
func (c *Client) explainHandshake(err error) error {
	var what string
	var netErr net.Error

	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		what = "server closed the connection during the handshake"
	case errors.As(err, &netErr) && netErr.Timeout():
		// Aiming the agent at an HTTPS port produces this: the TLS handshake
		// succeeds because it really is a TLS server, and then nothing ever
		// answers the tunnel protocol.
		what = "server accepted the connection but never answered the handshake"
	default:
		return err
	}

	hint := "; check that " + c.cfg.ServerAddr +
		" is the control port (7000 by default) and not the HTTPS port"
	if !c.cfg.TLS {
		hint = "; the server may require TLS — run `burrow login` again without -no-tls"
	}
	return fmt.Errorf("%s%s: %w", what, hint, err)
}

// dialServer opens the raw control connection.
func (c *Client) dialServer(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.cfg.ServerAddr, err)
	}
	if !c.cfg.TLS {
		return conn, nil
	}

	name := c.cfg.TLSServerName
	if name == "" {
		name, _, _ = net.SplitHostPort(c.cfg.ServerAddr)
	}
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         name,
		InsecureSkipVerify: c.cfg.Insecure,
		MinVersion:         tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS handshake with %s: %w", c.cfg.ServerAddr, err)
	}
	return tlsConn, nil
}

// handshake authenticates against the server.
func (c *Client) handshake(ctl net.Conn) error {
	hostname, _ := os.Hostname()
	_ = ctl.SetDeadline(time.Now().Add(20 * time.Second))
	defer ctl.SetDeadline(time.Time{})

	if err := proto.Write(ctl, proto.TypeHello, proto.Hello{
		Version:  proto.Version,
		Token:    c.cfg.Token,
		Agent:    c.cfg.Version,
		Hostname: hostname,
	}); err != nil {
		return err
	}
	var resp proto.HelloResp
	if err := proto.ReadAs(ctl, proto.TypeHelloResp, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%w: %s", ErrRejected, resp.Error)
	}
	if c.cfg.OnAuthorizedKeys != nil {
		c.cfg.OnAuthorizedKeys(resp.SSHAuthorizedKeys)
	}
	c.log.Debug("handshake ok", "server", resp.Server,
		"base_domain", resp.BaseDomain, "ssh_keys", len(resp.SSHAuthorizedKeys))
	return nil
}

// register asks for every configured tunnel and records the routes.
func (c *Client) register(ctl net.Conn) ([]proto.TunnelResp, error) {
	_ = ctl.SetDeadline(time.Now().Add(30 * time.Second))
	defer ctl.SetDeadline(time.Time{})

	routes := make(map[string]string, len(c.cfg.Tunnels))
	granted := make([]proto.TunnelResp, 0, len(c.cfg.Tunnels))
	var failures []string

	for _, spec := range c.cfg.Tunnels {
		if err := proto.Write(ctl, proto.TypeTunnelReq, proto.TunnelReq{
			Proto:      spec.Proto,
			Subdomain:  spec.Subdomain,
			RemotePort: spec.RemotePort,
			LocalAddr:  spec.LocalAddr,
		}); err != nil {
			return nil, err
		}
		var resp proto.TunnelResp
		if err := proto.ReadAs(ctl, proto.TypeTunnelResp, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			failures = append(failures, fmt.Sprintf("%s %s: %s", spec.Proto, spec.LocalAddr, resp.Error))
			continue
		}
		routes[resp.ID] = spec.LocalAddr
		granted = append(granted, resp)
	}

	for _, f := range failures {
		c.log.Error("tunnel refused", "detail", f)
	}
	if len(granted) == 0 {
		// Not ErrRejected: a name can be held by our own session that the
		// server has not reaped yet, and that clears on its own.
		return nil, fmt.Errorf("no tunnel could be opened")
	}

	c.mu.Lock()
	c.routes = routes
	c.mu.Unlock()
	return granted, nil
}

// acceptStreams serves data streams the server opens toward us.
func (c *Client) acceptStreams(mux *yamux.Session) error {
	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			return fmt.Errorf("accept stream: %w", err)
		}
		go c.handleStream(stream)
	}
}

// handleStream connects one incoming stream to the local service.
func (c *Client) handleStream(stream net.Conn) {
	defer stream.Close()

	_ = stream.SetDeadline(time.Now().Add(localDialTimeout + 5*time.Second))

	var open proto.StreamOpen
	if err := proto.ReadAs(stream, proto.TypeStreamOpen, &open); err != nil {
		c.log.Warn("bad stream header", "err", err)
		return
	}

	c.mu.RLock()
	local, ok := c.routes[open.TunnelID]
	c.mu.RUnlock()
	if !ok {
		_ = proto.Write(stream, proto.TypeStreamAck, proto.StreamAck{
			OK: false, Error: "unknown tunnel id " + open.TunnelID,
		})
		return
	}

	upstream, err := net.DialTimeout("tcp", local, localDialTimeout)
	if err != nil {
		c.log.Warn("local service unreachable", "local", local, "peer", open.RemoteAddr, "err", err)
		_ = proto.Write(stream, proto.TypeStreamAck, proto.StreamAck{OK: false, Error: err.Error()})
		return
	}
	defer upstream.Close()

	if err := proto.Write(stream, proto.TypeStreamAck, proto.StreamAck{OK: true}); err != nil {
		return
	}
	// Whatever follows may idle for hours (websocket, ssh); drop the deadline.
	_ = stream.SetDeadline(time.Time{})

	c.log.Debug("stream open", "local", local, "peer", open.RemoteAddr)
	netutil.Join(stream, upstream)
}

// slogWriter adapts yamux's io.Writer logging to slog at debug level.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	for len(p) > 0 && (p[len(p)-1] == '\n' || p[len(p)-1] == '\r') {
		p = p[:len(p)-1]
	}
	w.log.Debug("yamux: " + string(p))
	return len(p), nil
}
