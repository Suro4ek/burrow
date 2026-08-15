package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the server's runtime configuration. Zero values are not usable;
// build one with DefaultConfig and override.
type Config struct {
	// ControlAddr is where agents connect (the multiplexed control channel).
	ControlAddr string
	// HTTPAddr is where end users' HTTP requests arrive.
	HTTPAddr string
	// HTTPSAddr optionally serves the same traffic over TLS, using TLSCert
	// and TLSKey. With a wildcard certificate this removes the need for a
	// reverse proxy in front of the server.
	HTTPSAddr string
	// RedirectHTTPS turns the plain HTTP listener into a permanent redirect.
	// Safe with DNS-01 certificates; do not use it if something on port 80
	// still has to answer an HTTP-01 challenge.
	RedirectHTTPS bool
	// BaseDomain is the wildcard zone, e.g. "tun.example.com". Requests for
	// <sub>.tun.example.com are routed to the agent holding <sub>.
	BaseDomain string
	// PublicScheme is the scheme printed in tunnel URLs. Set it to "https"
	// when a TLS terminator (Caddy, nginx) sits in front of HTTPAddr.
	PublicScheme string
	// PublicHost is the host printed for TCP tunnels. Defaults to BaseDomain.
	PublicHost string

	// TCPBind is the interface TCP tunnel listeners bind to.
	TCPBind string
	// TCPPortMin/TCPPortMax bound the pool of public TCP ports.
	TCPPortMin int
	TCPPortMax int

	// TokensFile is a JSON array of Token records.
	TokensFile string
	// FreeSubdomains lets any authenticated agent claim any unreserved name.
	// Turn it off to restrict every agent to its own reservations.
	FreeSubdomains bool
	// FreePorts is the TCP equivalent of FreeSubdomains: when false, agents
	// may only request ports reserved to them (random allocation still works).
	FreePorts bool

	// TLSCert/TLSKey enable TLS on the control listener. Strongly recommended:
	// without it the agent token crosses the network in plaintext.
	TLSCert string
	TLSKey  string

	// StatusToken guards GET /_status on the base domain. Empty disables it.
	StatusToken string

	// AdminPassword enables the web panel at /_admin on the base domain.
	// Empty disables the panel and its API entirely.
	AdminPassword string
	// AdminAddr optionally serves the same panel on a second listener. Bind
	// it to 127.0.0.1 and reach it over an SSH tunnel to keep the panel off
	// the public internet.
	AdminAddr string

	// MaxTunnelsPerSession caps tunnels on one agent connection.
	MaxTunnelsPerSession int

	// Routes send fixed hostnames to a local upstream instead of to a tunnel,
	// so a panel or a status page can share the TLS listener. Keys are
	// hostnames, values are host:port.
	Routes map[string]string

	// AuthHookURL delegates agent authentication to an external control
	// plane instead of the local token file. This is the seam a hosted
	// service builds on: accounts, plans and billing stay outside the tunnel
	// path entirely.
	AuthHookURL string
	// UsageHookURL receives periodic traffic reports, and may answer with
	// identities to disconnect — the enforcement path for quotas.
	UsageHookURL string
	// HookToken authenticates burrowd to the control plane.
	HookToken string
	// HookTimeout bounds a single control-plane request.
	HookTimeout time.Duration
	// HookCacheTTL is how long an authenticated identity stays usable
	// without the agent reconnecting.
	HookCacheTTL time.Duration
	// UsageInterval is how often usage is reported.
	UsageInterval time.Duration
}

// DefaultConfig returns a configuration that only needs BaseDomain and
// TokensFile filled in.
func DefaultConfig() Config {
	return Config{
		ControlAddr:          ":7000",
		HTTPAddr:             ":80",
		PublicScheme:         "http",
		TCPBind:              "0.0.0.0",
		TCPPortMin:           20000,
		TCPPortMax:           30000,
		FreeSubdomains:       true,
		FreePorts:            false,
		MaxTunnelsPerSession: 16,
		HookTimeout:          10 * time.Second,
		HookCacheTTL:         12 * time.Hour,
		UsageInterval:        60 * time.Second,
	}
}

// Validate checks the configuration and fills in derived defaults.
func (c *Config) Validate() error {
	c.BaseDomain = strings.ToLower(strings.Trim(strings.TrimSpace(c.BaseDomain), "."))
	if c.BaseDomain == "" {
		return fmt.Errorf("base-domain is required (e.g. tun.example.com)")
	}
	if !strings.Contains(c.BaseDomain, ".") {
		return fmt.Errorf("base-domain %q does not look like a domain", c.BaseDomain)
	}
	if c.ControlAddr == "" {
		return fmt.Errorf("control-addr is required")
	}
	switch c.PublicScheme {
	case "http", "https":
	case "":
		c.PublicScheme = "http"
	default:
		return fmt.Errorf("public-scheme must be http or https, got %q", c.PublicScheme)
	}
	if c.PublicHost == "" {
		c.PublicHost = c.BaseDomain
	}
	if c.TCPPortMin > c.TCPPortMax {
		return fmt.Errorf("tcp port range %d-%d is inverted", c.TCPPortMin, c.TCPPortMax)
	}
	if c.TCPPortMin < 1 || c.TCPPortMax > 65535 {
		return fmt.Errorf("tcp port range %d-%d is outside 1-65535", c.TCPPortMin, c.TCPPortMax)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("tls-cert and tls-key must be set together")
	}
	if c.HTTPSAddr != "" && c.TLSCert == "" {
		return fmt.Errorf("https requires tls-cert and tls-key")
	}
	if c.RedirectHTTPS && c.HTTPSAddr == "" {
		return fmt.Errorf("redirect-https requires an https listener")
	}
	// Serving TLS and then advertising http:// URLs would hand every agent a
	// link that redirects, so follow the listener unless told otherwise.
	if c.HTTPSAddr != "" && c.PublicScheme == "http" {
		c.PublicScheme = "https"
	}
	if c.TokensFile == "" {
		return fmt.Errorf("tokens file is required")
	}
	for host := range c.Routes {
		// A routed name inside the tunnel zone would shadow a tunnel, and the
		// person who claimed that name would never find out why theirs is
		// unreachable.
		if strings.HasSuffix(host, "."+c.BaseDomain) {
			return fmt.Errorf("route %q is inside the tunnel zone and would shadow a tunnel", host)
		}
	}
	if c.MaxTunnelsPerSession <= 0 {
		c.MaxTunnelsPerSession = 16
	}
	if c.HookTimeout <= 0 {
		c.HookTimeout = 10 * time.Second
	}
	if c.HookCacheTTL <= 0 {
		c.HookCacheTTL = 12 * time.Hour
	}
	if c.UsageInterval <= 0 {
		c.UsageInterval = 60 * time.Second
	}
	if c.UsageHookURL != "" && c.AuthHookURL == "" {
		// Usage is attributed to identity ids, which only the auth hook can
		// mint; reporting against local token ids would be meaningless to a
		// control plane that never saw them.
		return fmt.Errorf("usage-hook-url requires auth-hook-url")
	}
	return nil
}

// ParsePortRange parses "20000-30000".
func ParsePortRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("port range %q must look like 20000-30000", s)
	}
	if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
		return 0, 0, fmt.Errorf("port range %q: %w", s, err)
	}
	if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
		return 0, 0, fmt.Errorf("port range %q: %w", s, err)
	}
	return lo, hi, nil
}
