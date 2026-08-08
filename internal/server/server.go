package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is the VPS-side daemon: it accepts agent connections on the control
// listener and publishes their local services over HTTP and raw TCP.
type Server struct {
	cfg     *Config
	store   *Store
	admin   *adminAuth
	adminH  http.Handler
	reg     *Registry
	log     *slog.Logger
	version string
	started time.Time

	// ready is closed once both listeners are bound, at which point ctlAddr
	// and httpAddr are safe to read. This is what lets callers use ":0" and
	// still learn where the server ended up.
	ready    chan struct{}
	ctlAddr  net.Addr
	httpAddr net.Addr

	mu       sync.Mutex
	sessions map[string]*Session
}

// Ready is closed when both listeners are accepting.
func (srv *Server) Ready() <-chan struct{} { return srv.ready }

// ControlAddr returns the bound agent-facing address. Valid after Ready.
func (srv *Server) ControlAddr() net.Addr { return srv.ctlAddr }

// HTTPAddr returns the bound end-user HTTP address. Valid after Ready.
func (srv *Server) HTTPAddr() net.Addr { return srv.httpAddr }

// New validates the configuration, loads tokens and returns a ready server.
func New(cfg Config, log *slog.Logger, version string) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store, err := OpenStore(cfg.TokensFile)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	srv := &Server{
		cfg:      &cfg,
		store:    store,
		reg:      NewRegistry(&cfg),
		log:      log,
		version:  version,
		started:  time.Now(),
		ready:    make(chan struct{}),
		sessions: make(map[string]*Session),
	}

	if cfg.AdminPassword != "" {
		if srv.admin, err = newAdminAuth(cfg.AdminPassword); err != nil {
			return nil, err
		}
		srv.adminH = srv.adminRoutes()
	}
	return srv, nil
}

// AdminEnabled reports whether the panel and its API are served.
func (srv *Server) AdminEnabled() bool { return srv.adminH != nil }

// Run serves until ctx is cancelled or a listener fails.
func (srv *Server) Run(ctx context.Context) error {
	ctlLn, err := srv.listenControl()
	if err != nil {
		return err
	}
	defer ctlLn.Close()

	httpLn, err := net.Listen("tcp", srv.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", srv.cfg.HTTPAddr, err)
	}
	defer httpLn.Close()

	httpSrv := &http.Server{
		Handler: srv,
		// No WriteTimeout on purpose: it would cut off websockets, SSE and
		// long downloads, which are exactly what people tunnel.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(srv.log.Handler(), slog.LevelDebug),
	}

	srv.ctlAddr, srv.httpAddr = ctlLn.Addr(), httpLn.Addr()
	close(srv.ready)

	srv.log.Info("tunnel server starting",
		"version", srv.version,
		"control", srv.ctlAddr.String(),
		"control_tls", srv.cfg.TLSCert != "",
		"http", srv.httpAddr.String(),
		"base_domain", srv.cfg.BaseDomain,
		"tcp_range", fmt.Sprintf("%d-%d", srv.cfg.TCPPortMin, srv.cfg.TCPPortMax),
		"admin", srv.AdminEnabled(),
		"tokens", srv.store.Count(),
	)
	if srv.AdminEnabled() {
		srv.log.Info("admin panel",
			"url", fmt.Sprintf("%s://%s/_admin/", srv.cfg.PublicScheme, srv.cfg.BaseDomain))
	} else {
		srv.log.Info("admin panel disabled: set -admin-password to enable it")
	}

	errc := make(chan error, 3)
	go func() {
		errc <- fmt.Errorf("http listener: %w", httpSrv.Serve(httpLn))
	}()
	go func() {
		errc <- fmt.Errorf("control listener: %w", srv.acceptControl(ctlLn))
	}()

	// The optional second admin listener answers on any Host, so it works
	// through an SSH port-forward where the Host header says "localhost".
	var adminSrv *http.Server
	if srv.cfg.AdminAddr != "" && srv.adminH != nil {
		adminLn, err := net.Listen("tcp", srv.cfg.AdminAddr)
		if err != nil {
			return fmt.Errorf("listen admin %s: %w", srv.cfg.AdminAddr, err)
		}
		defer adminLn.Close()
		adminSrv = &http.Server{
			Handler:           srv.adminH,
			ReadHeaderTimeout: 20 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          slog.NewLogLogger(srv.log.Handler(), slog.LevelDebug),
		}
		srv.log.Info("admin panel listener", "addr", adminLn.Addr().String())
		go func() {
			errc <- fmt.Errorf("admin listener: %w", adminSrv.Serve(adminLn))
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		srv.log.Info("shutting down")
	case runErr = <-errc:
	}

	_ = ctlLn.Close()
	srv.closeSessions()

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutCtx)
	}

	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) && !errors.Is(runErr, net.ErrClosed) {
		return runErr
	}
	return nil
}

// listenControl opens the agent-facing listener, wrapped in TLS when
// configured.
func (srv *Server) listenControl() (net.Listener, error) {
	ln, err := net.Listen("tcp", srv.cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("listen control %s: %w", srv.cfg.ControlAddr, err)
	}
	if srv.cfg.TLSCert == "" {
		srv.log.Warn("control listener has no TLS: agent tokens will cross the network in plaintext")
		return ln, nil
	}
	reloader, err := newCertReloader(srv.cfg.TLSCert, srv.cfg.TLSKey)
	if err != nil {
		ln.Close()
		return nil, err
	}
	return tls.NewListener(ln, reloader.tlsConfig()), nil
}

// acceptControl runs the agent accept loop.
func (srv *Server) acceptControl(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		go srv.handleAgent(conn)
	}
}

func (srv *Server) addSession(s *Session) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.sessions[s.ID] = s
}

func (srv *Server) removeSession(s *Session) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	delete(srv.sessions, s.ID)
}

// closeSessions drops every connected agent, which releases their tunnels.
func (srv *Server) closeSessions() {
	srv.mu.Lock()
	sessions := make([]*Session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		sessions = append(sessions, s)
	}
	srv.mu.Unlock()

	for _, s := range sessions {
		s.Close()
	}
}
