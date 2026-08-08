package server

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"time"

	"github.com/suro4ek/burrow/internal/netutil"
)

// Tunnel is one published endpoint belonging to one agent session.
type Tunnel struct {
	ID        string
	Proto     string // proto.ProtoHTTP or proto.ProtoTCP
	Subdomain string // http only
	Port      int    // tcp only
	LocalAddr string // what the agent reports it forwards to, informational
	Created   time.Time

	// Traffic and Conns feed the admin panel; both are updated from many
	// goroutines at once.
	Traffic netutil.Counter
	Conns   atomic.Int64

	sess  *Session
	proxy *httputil.ReverseProxy // http only
	ln    net.Listener           // tcp only
}

// Session returns the agent session serving this tunnel.
func (t *Tunnel) Session() *Session { return t.sess }

// Public renders the tunnel's public address for humans.
func (t *Tunnel) Public(cfg *Config) string {
	if t.Proto == "tcp" {
		return fmt.Sprintf("%s:%d", cfg.PublicHost, t.Port)
	}
	return fmt.Sprintf("%s://%s.%s", cfg.PublicScheme, t.Subdomain, cfg.BaseDomain)
}

// ErrTaken is returned when a requested name or port is already serving.
var ErrTaken = errors.New("already in use")

// Registry tracks live tunnels and allocates public names and ports.
//
// Allocation and binding happen under the same lock: a TCP port is only
// recorded once its listener is actually bound, so two agents racing for the
// same port can never both believe they won.
type Registry struct {
	cfg *Config

	mu     sync.RWMutex
	byHTTP map[string]*Tunnel
	byTCP  map[int]*Tunnel
	byID   map[string]*Tunnel
}

// NewRegistry returns an empty registry.
func NewRegistry(cfg *Config) *Registry {
	return &Registry{
		cfg:    cfg,
		byHTTP: make(map[string]*Tunnel),
		byTCP:  make(map[int]*Tunnel),
		byID:   make(map[string]*Tunnel),
	}
}

// LookupHTTP finds the tunnel serving a subdomain.
func (r *Registry) LookupHTTP(sub string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byHTTP[sub]
}

// List returns a snapshot of all live tunnels.
func (r *Registry) List() []*Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tunnel, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t)
	}
	return out
}

// Count returns the number of live tunnels.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// ClaimHTTP registers t under sub, or under a fresh random name when sub is
// empty. The chosen subdomain is written back to t.
func (r *Registry) ClaimHTTP(t *Tunnel, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sub != "" {
		if _, taken := r.byHTTP[sub]; taken {
			return fmt.Errorf("subdomain %q: %w", sub, ErrTaken)
		}
	} else {
		// 8 random characters from a 32-symbol alphabet is ~40 bits; a
		// handful of retries covers any realistic collision.
		for i := 0; ; i++ {
			candidate := netutil.RandID(8)
			if _, taken := r.byHTTP[candidate]; !taken && !reservedSubdomains[candidate] {
				sub = candidate
				break
			}
			if i > 32 {
				return errors.New("could not allocate a free subdomain")
			}
		}
	}

	t.Subdomain = sub
	r.byHTTP[sub] = t
	r.byID[t.ID] = t
	return nil
}

// FirstFreePort returns the first candidate that is inside the configured
// range and not currently serving, or 0 when none qualifies. The answer is
// advisory: ClaimTCP re-checks under the lock.
func (r *Registry) FirstFreePort(candidates []int) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range candidates {
		if p < r.cfg.TCPPortMin || p > r.cfg.TCPPortMax {
			continue
		}
		if _, taken := r.byTCP[p]; !taken {
			return p
		}
	}
	return 0
}

// LookupID finds a tunnel by ID.
func (r *Registry) LookupID(id string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// ClaimTCP binds a public TCP port for t. When port is 0 a free port is picked
// from the configured range. The bound listener is stored on t and returned.
func (r *Registry) ClaimTCP(t *Tunnel, port int) (net.Listener, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	bind := func(p int) (net.Listener, error) {
		if _, taken := r.byTCP[p]; taken {
			return nil, fmt.Errorf("port %d: %w", p, ErrTaken)
		}
		return net.Listen("tcp", net.JoinHostPort(r.cfg.TCPBind, fmt.Sprint(p)))
	}

	var ln net.Listener
	var err error
	if port != 0 {
		if port < r.cfg.TCPPortMin || port > r.cfg.TCPPortMax {
			return nil, fmt.Errorf("port %d is outside the allowed range %d-%d",
				port, r.cfg.TCPPortMin, r.cfg.TCPPortMax)
		}
		if ln, err = bind(port); err != nil {
			return nil, err
		}
	} else {
		span := r.cfg.TCPPortMax - r.cfg.TCPPortMin + 1
		// Probe random ports rather than scanning: the range is sparse in
		// practice and this avoids clustering every agent at the low end.
		attempts := min(span, 64)
		for range attempts {
			p := r.cfg.TCPPortMin + rand.IntN(span)
			// A port can be free in our map yet held by another process on
			// the host, so a failed bind just means "try the next one".
			if ln, err = bind(p); err == nil {
				port = p
				break
			}
		}
		if ln == nil {
			return nil, fmt.Errorf("no free TCP port in range %d-%d", r.cfg.TCPPortMin, r.cfg.TCPPortMax)
		}
	}

	t.Port = port
	t.ln = ln
	r.byTCP[port] = t
	r.byID[t.ID] = t
	return ln, nil
}

// Release removes a tunnel and closes its TCP listener, if any. It is safe to
// call more than once.
func (r *Registry) Release(t *Tunnel) {
	r.mu.Lock()
	if cur, ok := r.byHTTP[t.Subdomain]; ok && cur == t {
		delete(r.byHTTP, t.Subdomain)
	}
	if cur, ok := r.byTCP[t.Port]; ok && cur == t {
		delete(r.byTCP, t.Port)
	}
	delete(r.byID, t.ID)
	ln := t.ln
	t.ln = nil
	r.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
}
