package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// newProxy builds a reverse proxy dedicated to one tunnel.
//
// One Transport per tunnel is deliberate: http.Transport pools connections by
// address, and every tunnel dials the same synthetic address. A shared
// Transport would hand tunnel A's pooled connection to tunnel B.
func (srv *Server) newProxy(t *Tunnel) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return t.dial(ctx)
		},
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
		// Let the client's own Accept-Encoding pass through untouched; a
		// tunnel should not silently change what the origin receives.
		DisableCompression: true,
		// The tunnel is not a real TLS endpoint, so ALPN-negotiated HTTP/2
		// makes no sense here. Requests reach the agent as HTTP/1.1.
		ForceAttemptHTTP2: false,
	}

	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// Ignored by DialContext, but net/http insists on a valid host.
			pr.Out.URL.Host = "tunnel.invalid"
			pr.SetXForwarded()
			// Preserve the Host the user typed: name-based vhosts and
			// absolute-URL generation on the origin depend on it.
			pr.Out.Host = pr.In.Host
			pr.Out = pr.Out.WithContext(withRemoteAddr(pr.Out.Context(), pr.In.RemoteAddr))
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			srv.log.Warn("proxy error",
				"tunnel", t.ID, "host", r.Host, "path", r.URL.Path, "err", err)
			writeErrorPage(w, http.StatusBadGateway,
				"Tunnel is connected, but the local service did not answer",
				err.Error())
		},
	}
}

// ServeHTTP routes an end user's request to the right tunnel by Host header.
func (srv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)

	if host == srv.cfg.BaseDomain {
		// The panel lives on the bare base domain, so a tunnel can never
		// shadow it: subdomains are a different host entirely.
		if srv.adminH != nil && isAdminPath(r.URL.Path) {
			srv.adminH.ServeHTTP(w, r)
			return
		}
		srv.serveBaseDomain(w, r)
		return
	}

	sub, ok := srv.subdomainOf(host)
	if !ok {
		writeErrorPage(w, http.StatusNotFound, "Unknown host",
			fmt.Sprintf("%s is not served by this tunnel server.", host))
		return
	}

	t := srv.reg.LookupHTTP(sub)
	if t == nil {
		writeErrorPage(w, http.StatusNotFound, "Tunnel not found",
			fmt.Sprintf("No agent is currently serving %q. The tunnel may have been closed.", sub))
		return
	}

	t.proxy.ServeHTTP(w, r)
}

// isAdminPath reports whether a path belongs to the admin panel or its API.
func isAdminPath(p string) bool {
	return p == "/_admin" || strings.HasPrefix(p, "/_admin/") || strings.HasPrefix(p, "/_api/")
}

// serveBaseDomain answers requests aimed at the bare base domain.
func (srv *Server) serveBaseDomain(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/_health":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	case "/_status":
		srv.serveStatus(w, r)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, basePage, html.EscapeString(srv.cfg.BaseDomain), srv.reg.Count())
	}
}

// statusTunnel is the JSON shape of one tunnel in the status report.
type statusTunnel struct {
	ID        string `json:"id"`
	Proto     string `json:"proto"`
	Public    string `json:"public"`
	Local     string `json:"local"`
	Token     string `json:"token"`
	Hostname  string `json:"hostname,omitempty"`
	AgentAddr string `json:"agent_addr"`
	AgeSec    int    `json:"age_sec"`
}

// serveStatus reports live tunnels. It is gated behind StatusToken because the
// listing leaks every active subdomain on the server.
func (srv *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	if srv.cfg.StatusToken == "" {
		http.NotFound(w, r)
		return
	}
	given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if given == "" {
		given = r.URL.Query().Get("token")
	}
	if given != srv.cfg.StatusToken {
		w.Header().Set("WWW-Authenticate", `Bearer realm="status"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	out := make([]statusTunnel, 0, srv.reg.Count())
	for _, t := range srv.reg.List() {
		s := t.Session()
		out = append(out, statusTunnel{
			ID:        t.ID,
			Proto:     t.Proto,
			Public:    t.Public(srv.cfg),
			Local:     t.LocalAddr,
			Token:     s.TokenName,
			Hostname:  s.Hostname,
			AgentAddr: s.RemoteAddr,
			AgeSec:    int(time.Since(t.Created).Seconds()),
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"tunnels": out})
}

// subdomainOf extracts the single label in front of the base domain.
func (srv *Server) subdomainOf(host string) (string, bool) {
	suffix := "." + srv.cfg.BaseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	sub := strings.TrimSuffix(host, suffix)
	// Only one level deep: a.b.tun.example.com is not a valid tunnel host, and
	// treating it as one would let a stale wildcard cert cover more than
	// intended.
	if sub == "" || strings.Contains(sub, ".") {
		return "", false
	}
	return sub, true
}

// normalizeSubdomain lowercases and trims a requested name.
func normalizeSubdomain(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// hostOnly strips an optional port and lowercases a Host header value.
func hostOnly(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// writeErrorPage renders a plain, dependency-free error page. It answers with
// JSON when the caller clearly is not a browser.
func writeErrorPage(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, errorPage, code, html.EscapeString(title), html.EscapeString(detail))
}

const basePage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%[1]s</title>
<style>
 body{font:16px/1.6 ui-sans-serif,system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1.5rem;color:#111}
 code{background:#f4f4f5;padding:.15em .4em;border-radius:.25rem}
 @media (prefers-color-scheme:dark){body{background:#0b0b0c;color:#e7e7e9}code{background:#1c1c1f}}
</style>
<h1>%[1]s</h1>
<p>Tunnel server is running. %[2]d tunnel(s) currently connected.</p>
<p>Point an agent at this host to publish a local service.</p>
`

const errorPage = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%[1]d %[2]s</title>
<style>
 body{font:16px/1.6 ui-sans-serif,system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1.5rem;color:#111}
 .code{font-size:3rem;font-weight:600;color:#a1a1aa;margin:0}
 pre{background:#f4f4f5;padding:1rem;border-radius:.5rem;overflow-x:auto;white-space:pre-wrap}
 @media (prefers-color-scheme:dark){body{background:#0b0b0c;color:#e7e7e9}pre{background:#1c1c1f}}
</style>
<p class="code">%[1]d</p>
<h1>%[2]s</h1>
<pre>%[3]s</pre>
`
