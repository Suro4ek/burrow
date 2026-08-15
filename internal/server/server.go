package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"
)

// Server is the VPS-side daemon: it accepts agent connections on the control
// listener and publishes their local services over HTTP and raw TCP.
type Server struct {
	cfg     *Config
	store   *Store
	auth    Authenticator
	usage   *usageReporter
	admin   *adminAuth
	adminH  http.Handler
	reg     *Registry
	routes  map[string]*httputil.ReverseProxy
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

	// The token file stays the default. An auth hook replaces it, which is how
	// a hosted control plane owns accounts without any of that reaching here.
	srv.auth = storeAuth{store: store}
	if cfg.AuthHookURL != "" {
		hook := newHookClient(&cfg, log, version)
		srv.auth = hook
		if cfg.UsageHookURL != "" {
			srv.usage = &usageReporter{srv: srv, hook: hook}
		}
	}

	srv.routes = srv.buildRoutes()

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
	// One reloader shared by every TLS listener: the certificate is the same,
	// and sharing means a renewal is picked up everywhere at once.
	var certs *certReloader
	if srv.cfg.TLSCert != "" {
		var err error
		if certs, err = newCertReloader(srv.cfg.TLSCert, srv.cfg.TLSKey); err != nil {
			return err
		}
	}

	ctlLn, err := srv.listenControl(certs)
	if err != nil {
		return err
	}
	defer ctlLn.Close()

	httpLn, err := net.Listen("tcp", srv.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen http %s: %w", srv.cfg.HTTPAddr, err)
	}
	defer httpLn.Close()

	newHTTPServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler: h,
			// No WriteTimeout on purpose: it would cut off websockets, SSE and
			// long downloads, which are exactly what people tunnel.
			ReadHeaderTimeout: 20 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          slog.NewLogLogger(srv.log.Handler(), slog.LevelDebug),
		}
	}

	plain := http.Handler(srv)
	if srv.cfg.RedirectHTTPS {
		plain = http.HandlerFunc(srv.redirectToHTTPS)
	}
	httpSrv := newHTTPServer(plain)

	srv.ctlAddr, srv.httpAddr = ctlLn.Addr(), httpLn.Addr()
	close(srv.ready)

	srv.log.Info("tunnel server starting",
		"version", srv.version,
		"control", srv.ctlAddr.String(),
		"control_tls", srv.cfg.TLSCert != "",
		"http", srv.httpAddr.String(),
		"https", srv.cfg.HTTPSAddr,
		"base_domain", srv.cfg.BaseDomain,
		"tcp_range", fmt.Sprintf("%d-%d", srv.cfg.TCPPortMin, srv.cfg.TCPPortMax),
		"admin", srv.AdminEnabled(),
		"routes", len(srv.routes),
		"tokens", srv.store.Count(),
		"auth_hook", srv.cfg.AuthHookURL != "",
	)
	if srv.AdminEnabled() {
		srv.log.Info("admin panel",
			"url", fmt.Sprintf("%s://%s/_admin/", srv.cfg.PublicScheme, srv.cfg.BaseDomain))
	} else {
		srv.log.Info("admin panel disabled: set -admin-password to enable it")
	}

	errc := make(chan error, 4)
	go func() {
		errc <- fmt.Errorf("http listener: %w", httpSrv.Serve(httpLn))
	}()
	go func() {
		errc <- fmt.Errorf("control listener: %w", srv.acceptControl(ctlLn))
	}()

	// Serving TLS here is what lets a wildcard certificate replace a reverse
	// proxy entirely: one binary terminates the tunnels and the panel.
	var httpsSrv *http.Server
	if srv.cfg.HTTPSAddr != "" {
		rawLn, err := net.Listen("tcp", srv.cfg.HTTPSAddr)
		if err != nil {
			return fmt.Errorf("listen https %s: %w", srv.cfg.HTTPSAddr, err)
		}
		httpsLn := tls.NewListener(rawLn, certs.tlsConfig())
		defer httpsLn.Close()

		httpsSrv = newHTTPServer(srv)
		srv.log.Info("https listener", "addr", rawLn.Addr().String(),
			"redirect_from_http", srv.cfg.RedirectHTTPS)
		go func() {
			errc <- fmt.Errorf("https listener: %w", httpsSrv.Serve(httpsLn))
		}()
	}

	if srv.usage != nil {
		srv.log.Info("usage reporting enabled",
			"url", srv.cfg.UsageHookURL, "interval", srv.cfg.UsageInterval)
		usageCtx, stopUsage := context.WithCancel(context.Background())
		defer stopUsage()
		go srv.usage.run(usageCtx, srv.cfg.UsageInterval)
	}

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
	if httpsSrv != nil {
		_ = httpsSrv.Shutdown(shutCtx)
	}
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutCtx)
	}

	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) && !errors.Is(runErr, net.ErrClosed) {
		return runErr
	}
	return nil
}

// listenControl opens the agent-facing listener, wrapped in TLS when a
// certificate is available.
func (srv *Server) listenControl(certs *certReloader) (net.Listener, error) {
	ln, err := net.Listen("tcp", srv.cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("listen control %s: %w", srv.cfg.ControlAddr, err)
	}
	if certs == nil {
		srv.log.Warn("control listener has no TLS: agent tokens will cross the network in plaintext")
		return ln, nil
	}
	return tls.NewListener(ln, certs.tlsConfig()), nil
}

// redirectToHTTPS sends plain HTTP traffic to the TLS listener.
func (srv *Server) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	// Keep a non-standard HTTPS port in the redirect, or the browser would be
	// sent to :443 where nothing is listening.
	if _, port, err := net.SplitHostPort(srv.cfg.HTTPSAddr); err == nil && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
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
