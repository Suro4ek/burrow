package server

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// buildRoutes prepares the fixed host -> local upstream proxies.
//
// This is how the control panel gets a name of its own without a second
// daemon in front of everything: burrowd already owns :443 and already routes
// by Host, so pointing one name at a local address is a few lines rather than
// an extra process, an extra config file and an extra place TLS can break.
func (srv *Server) buildRoutes() map[string]*httputil.ReverseProxy {
	if len(srv.cfg.Routes) == 0 {
		return nil
	}
	out := make(map[string]*httputil.ReverseProxy, len(srv.cfg.Routes))

	for host, upstream := range srv.cfg.Routes {
		transport := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   false,
		}
		out[strings.ToLower(host)] = &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = upstream
				pr.SetXForwarded()
				// Keep the name the user typed: the upstream decides cookie
				// scope and absolute URLs from it.
				pr.Out.Host = pr.In.Host
			},
			ErrorLog: nil,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				srv.log.Warn("route failed", "host", r.Host, "upstream", upstream, "err", err)
				writeErrorPage(w, http.StatusBadGateway,
					"Service unavailable", "Nothing answered at "+upstream+".")
			},
		}
	}
	return out
}

// routeFor returns the proxy for a routed host, if there is one.
func (srv *Server) routeFor(host string) *httputil.ReverseProxy {
	if srv.routes == nil {
		return nil
	}
	return srv.routes[host]
}

// ParseRoute parses "host=127.0.0.1:8081".
func ParseRoute(s string) (host, upstream string, err error) {
	host, upstream, ok := strings.Cut(s, "=")
	host = strings.ToLower(strings.TrimSpace(host))
	upstream = strings.TrimSpace(upstream)
	if !ok || host == "" || upstream == "" {
		return "", "", fmt.Errorf("route %q must look like host.example.com=127.0.0.1:8081", s)
	}
	if strings.Contains(host, "/") {
		return "", "", fmt.Errorf("route %q: expected a hostname, not a URL", s)
	}
	if _, _, err := net.SplitHostPort(upstream); err != nil {
		return "", "", fmt.Errorf("route %q: upstream must be host:port", s)
	}
	return host, upstream, nil
}
