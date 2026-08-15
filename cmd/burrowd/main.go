// Command burrowd is the VPS-side tunnel server.
//
// It accepts agent connections on a control port and publishes their local
// services as <subdomain>.<base-domain> over HTTP and as public TCP ports.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/suro4ek/burrow/internal/server"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "gentoken" {
		genToken()
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "burrowd: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg := server.DefaultConfig()
	var (
		portRange = flag.String("tcp-range", "20000-30000", "public TCP port pool for tcp tunnels")
		adminPass = flag.String("admin-password-file", "", "file holding the admin panel password (env BURROWD_ADMIN_PASSWORD also works)")
		hookToken = flag.String("hook-token-file", "", "file holding the bearer token sent to the control plane (env BURROWD_HOOK_TOKEN also works)")
		logLevel  = flag.String("log-level", "info", "debug, info, warn or error")
		logJSON   = flag.Bool("log-json", false, "emit structured JSON logs")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.StringVar(&cfg.ControlAddr, "control", cfg.ControlAddr, "listen address for tunnel agents")
	flag.StringVar(&cfg.HTTPAddr, "http", cfg.HTTPAddr, "listen address for end-user HTTP traffic")
	flag.StringVar(&cfg.HTTPSAddr, "https", "", "listen address for end-user HTTPS traffic, e.g. :443 (needs tls-cert/tls-key)")
	flag.BoolVar(&cfg.RedirectHTTPS, "redirect-https", false, "make the http listener redirect to https")
	flag.StringVar(&cfg.BaseDomain, "domain", "", "wildcard base domain, e.g. tun.example.com (required)")
	flag.StringVar(&cfg.PublicScheme, "scheme", cfg.PublicScheme, "scheme used in published URLs: http or https")
	flag.StringVar(&cfg.PublicHost, "public-host", "", "hostname published for tcp tunnels (default: base domain)")
	flag.StringVar(&cfg.TCPBind, "tcp-bind", cfg.TCPBind, "interface that tcp tunnel listeners bind to")
	flag.StringVar(&cfg.TokensFile, "tokens", "tokens.json", "path to the tokens file")
	flag.StringVar(&cfg.TLSCert, "tls-cert", "", "TLS certificate for the control listener")
	flag.StringVar(&cfg.TLSKey, "tls-key", "", "TLS key for the control listener")
	flag.StringVar(&cfg.StatusToken, "status-token", "", "bearer token for GET /_status (empty disables it)")
	flag.StringVar(&cfg.AdminAddr, "admin-addr", "", "extra listener for the admin panel, e.g. 127.0.0.1:7002")
	flag.BoolVar(&cfg.FreeSubdomains, "free-subdomains", cfg.FreeSubdomains, "let any agent claim any unreserved subdomain")
	flag.BoolVar(&cfg.FreePorts, "free-ports", cfg.FreePorts, "let any agent request any unreserved fixed TCP port")
	flag.IntVar(&cfg.MaxTunnelsPerSession, "max-tunnels", cfg.MaxTunnelsPerSession, "tunnel limit per agent connection")
	flag.Var(&routeFlags{cfg: &cfg}, "route",
		"send a hostname to a local upstream, repeatable (e.g. app.example.com=127.0.0.1:8081)")
	flag.StringVar(&cfg.AuthHookURL, "auth-hook-url", "", "authenticate agents against this URL instead of the tokens file")
	flag.StringVar(&cfg.UsageHookURL, "usage-hook-url", "", "POST traffic reports to this URL (requires auth-hook-url)")
	flag.DurationVar(&cfg.UsageInterval, "usage-interval", cfg.UsageInterval, "how often to report usage")
	flag.DurationVar(&cfg.HookTimeout, "hook-timeout", cfg.HookTimeout, "timeout for a control-plane request")
	flag.DurationVar(&cfg.HookCacheTTL, "hook-cache-ttl", cfg.HookCacheTTL, "how long an authenticated identity stays usable without reconnecting")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `burrowd %s - self-hosted tunnel server

Usage:
  burrowd -domain tun.example.com -tokens /etc/burrowd/tokens.json [flags]
  burrowd gentoken

Flags:
`, version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVer {
		fmt.Println("burrowd", version)
		return nil
	}

	lo, hi, err := server.ParsePortRange(*portRange)
	if err != nil {
		return err
	}
	cfg.TCPPortMin, cfg.TCPPortMax = lo, hi

	// The password never arrives as a flag value: anything on the command
	// line is visible in `ps` to every user on the box.
	if cfg.AdminPassword, err = readSecret(*adminPass, "BURROWD_ADMIN_PASSWORD"); err != nil {
		return fmt.Errorf("admin password: %w", err)
	}
	if cfg.HookToken, err = readSecret(*hookToken, "BURROWD_HOOK_TOKEN"); err != nil {
		return fmt.Errorf("hook token: %w", err)
	}

	log, err := newLogger(*logLevel, *logJSON)
	if err != nil {
		return err
	}

	srv, err := server.New(cfg, log, version)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the context; a second signal aborts immediately
	// because stop() restores the default handler.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return srv.Run(ctx)
}

// routeFlags collects repeated -route flags.
type routeFlags struct{ cfg *server.Config }

func (r *routeFlags) String() string { return fmt.Sprintf("%d route(s)", len(r.cfg.Routes)) }

func (r *routeFlags) Set(v string) error {
	host, upstream, err := server.ParseRoute(v)
	if err != nil {
		return err
	}
	if r.cfg.Routes == nil {
		r.cfg.Routes = map[string]string{}
	}
	r.cfg.Routes[host] = upstream
	return nil
}

// readSecret resolves a secret from a file or the environment. Neither ever
// comes from a flag value: command-line arguments are visible in `ps` to every
// user on the machine.
func readSecret(path, env string) (string, error) {
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv(env)), nil
}

// newLogger builds the process logger.
func newLogger(level string, asJSON bool) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if asJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
}

// genToken prints a fresh agent token.
func genToken() {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		fmt.Fprintln(os.Stderr, "burrowd: cannot read random bytes:", err)
		os.Exit(1)
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(b))
}
