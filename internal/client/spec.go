package client

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/suro4ek/burrow/internal/proto"
)

// TunnelSpec describes one endpoint the agent wants published.
type TunnelSpec struct {
	Proto      string // proto.ProtoHTTP or proto.ProtoTCP
	LocalAddr  string // host:port the agent forwards to
	Subdomain  string // http: requested name, empty for a random one
	RemotePort int    // tcp: requested public port, 0 for any
}

// ParseLocalAddr accepts "3000", ":3000", "localhost:8080" or "10.0.0.5:5432"
// and returns a dialable host:port. A bare port means loopback, which is what
// people almost always mean and keeps the agent from exposing a LAN service by
// accident.
func ParseLocalAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty local address")
	}
	if port, err := strconv.Atoi(strings.TrimPrefix(s, ":")); err == nil {
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("port %d is outside 1-65535", port)
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("local address %q must be a port or host:port", s)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("local address %q has an invalid port", s)
	}
	return net.JoinHostPort(host, port), nil
}

// ParseSpec parses the compact form used by the repeatable --tunnel flag:
//
//	http:3000                  random subdomain
//	http:3000:myapp            fixed subdomain
//	http:localhost:8080        host:port local address
//	http:10.0.0.5:8080:myapp   both
//	tcp:22                     random public port
//	tcp:22:25343               fixed public port
//	tcp:10.0.0.5:5432          host:port local address
//	tcp:10.0.0.5:5432:25343    both
//
// The last field is a name for http and a port for tcp; a two-field form is a
// local host:port whenever the trailing field disambiguates it as one. IPv6
// literals are not accepted here — use the positional command form instead.
func ParseSpec(s string) (TunnelSpec, error) {
	kind, rest, ok := strings.Cut(s, ":")
	if !ok {
		return TunnelSpec{}, fmt.Errorf("tunnel spec %q must start with http: or tcp:", s)
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != proto.ProtoHTTP && kind != proto.ProtoTCP {
		return TunnelSpec{}, fmt.Errorf("unknown tunnel protocol %q, want http or tcp", kind)
	}

	parts := strings.Split(rest, ":")
	var local, last string
	switch len(parts) {
	case 1:
		local = parts[0]
	case 2:
		// "http:localhost:8080" is a local address; "http:3000:myapp" is a
		// local port plus a name. Only the first field tells them apart.
		if isPort(parts[0]) {
			local, last = parts[0], parts[1]
		} else {
			local = rest
		}
	case 3:
		local, last = parts[0]+":"+parts[1], parts[2]
	default:
		return TunnelSpec{}, fmt.Errorf("tunnel spec %q has too many fields", s)
	}

	addr, err := ParseLocalAddr(local)
	if err != nil {
		return TunnelSpec{}, fmt.Errorf("tunnel spec %q: %w", s, err)
	}
	spec := TunnelSpec{Proto: kind, LocalAddr: addr}

	if last != "" {
		if kind == proto.ProtoHTTP {
			spec.Subdomain = strings.ToLower(last)
		} else {
			port, err := strconv.Atoi(last)
			if err != nil || port < 1 || port > 65535 {
				return TunnelSpec{}, fmt.Errorf("tunnel spec %q: %q is not a valid public port", s, last)
			}
			spec.RemotePort = port
		}
	}
	return spec, nil
}

// isPort reports whether s is a bare port number.
func isPort(s string) bool {
	p, err := strconv.Atoi(s)
	return err == nil && p >= 1 && p <= 65535
}
