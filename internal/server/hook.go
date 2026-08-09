package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// maxHookBody caps what the control plane may send back, so a misbehaving one
// cannot exhaust memory here.
const maxHookBody = 1 << 20

// HookAuthRequest is the JSON burrowd POSTs to the auth endpoint.
type HookAuthRequest struct {
	Token         string `json:"token"`
	AgentVersion  string `json:"agent_version,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	RemoteAddr    string `json:"remote_addr,omitempty"`
	ServerVersion string `json:"server_version,omitempty"`
	BaseDomain    string `json:"base_domain,omitempty"`
}

// HookAuthResponse is what the control plane answers with.
//
// A 200 with ok=false is a clean rejection and Error is shown to the agent. A
// non-2xx status is treated as the control plane being broken, which is a
// different thing and is not reported to the agent as "invalid token".
type HookAuthResponse struct {
	OK         bool     `json:"ok"`
	Error      string   `json:"error,omitempty"`
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Subdomains []string `json:"subdomains,omitempty"`
	Ports      []int    `json:"ports,omitempty"`
	MaxTunnels int      `json:"max_tunnels,omitempty"`
	DenyTCP    bool     `json:"deny_tcp,omitempty"`
	SSHKeys    []string `json:"ssh_keys,omitempty"`
}

// HookUsageTunnel is one tunnel's lifetime totals.
//
// Totals rather than deltas on purpose: a dropped report then costs nothing,
// because the next one carries the full figure again. A control plane should
// keep the maximum it has seen per tunnel id.
type HookUsageTunnel struct {
	TunnelID  string    `json:"tunnel_id"`
	TokenID   string    `json:"token_id"`
	Proto     string    `json:"proto"`
	Public    string    `json:"public"`
	OpenedAt  time.Time `json:"opened_at"`
	Conns     int64     `json:"conns"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	Closed    bool      `json:"closed,omitempty"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
	AgentAddr string    `json:"agent_addr,omitempty"`
	Hostname  string    `json:"hostname,omitempty"`
}

// HookUsageRequest is the periodic report.
type HookUsageRequest struct {
	ServerVersion string            `json:"server_version,omitempty"`
	BaseDomain    string            `json:"base_domain,omitempty"`
	ReportedAt    time.Time         `json:"reported_at"`
	Tunnels       []HookUsageTunnel `json:"tunnels"`
}

// HookUsageResponse lets the control plane push decisions back.
//
// This is the enforcement path: it is how a cancelled subscription or an
// over-quota account stops using bandwidth without burrowd knowing anything
// about plans or payments.
type HookUsageResponse struct {
	// Disconnect lists identity ids whose agents should be dropped.
	Disconnect []string `json:"disconnect,omitempty"`
}

// hookClient talks to the control plane.
type hookClient struct {
	authURL  string
	usageURL string
	token    string
	http     *http.Client
	log      *slog.Logger
	version  string
	domain   string

	// mu guards the identity cache, which exists so that Refresh — called on
	// every tunnel request — does not become an HTTP round trip each time.
	mu     sync.Mutex
	cache  map[string]cachedIdentity
	cacheL time.Duration
}

type cachedIdentity struct {
	id  Identity
	exp time.Time
}

// newHookClient builds the client. Either URL may be empty.
func newHookClient(cfg *Config, log *slog.Logger, version string) *hookClient {
	return &hookClient{
		authURL:  cfg.AuthHookURL,
		usageURL: cfg.UsageHookURL,
		token:    cfg.HookToken,
		http:     &http.Client{Timeout: cfg.HookTimeout},
		log:      log,
		version:  version,
		domain:   cfg.BaseDomain,
		cache:    make(map[string]cachedIdentity),
		cacheL:   cfg.HookCacheTTL,
	}
}

// post sends v and decodes the reply into out.
func (h *hookClient) post(ctx context.Context, url string, v, out any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "burrowd/"+h.version)
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}

	resp, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("control plane returned %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxHookBody)).Decode(out)
}

// Authenticate asks the control plane who this token belongs to.
func (h *hookClient) Authenticate(ctx context.Context, req AuthRequest) (Identity, error) {
	var resp HookAuthResponse
	err := h.post(ctx, h.authURL, HookAuthRequest{
		Token:         req.Token,
		AgentVersion:  req.AgentVersion,
		Hostname:      req.Hostname,
		RemoteAddr:    req.RemoteAddr,
		ServerVersion: h.version,
		BaseDomain:    h.domain,
	}, &resp)
	if err != nil {
		// Do not tell the agent its token is bad when the truth is that the
		// control plane is unreachable; those need different reactions.
		h.log.Error("auth hook failed", "url", h.authURL, "err", err)
		return Identity{}, fmt.Errorf("authentication is temporarily unavailable")
	}
	if !resp.OK {
		reason := resp.Error
		if reason == "" {
			reason = "access denied"
		}
		return Identity{}, denied(reason)
	}
	if resp.ID == "" {
		h.log.Error("auth hook returned no id", "url", h.authURL)
		return Identity{}, fmt.Errorf("authentication is temporarily unavailable")
	}

	id := Identity{
		ID:         resp.ID,
		Name:       cmpOr(resp.Name, resp.ID),
		Subdomains: slices.Clone(resp.Subdomains),
		Ports:      slices.Clone(resp.Ports),
		MaxTunnels: resp.MaxTunnels,
		DenyTCP:    resp.DenyTCP,
		SSHKeys:    slices.Clone(resp.SSHKeys),
	}
	h.remember(id)
	return id, nil
}

// Refresh serves the cached identity, re-reading nothing: the control plane
// pushes changes through the usage response instead of being polled.
func (h *hookClient) Refresh(_ context.Context, id string) (Identity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.cache[id]
	if !ok || time.Now().After(c.exp) {
		return Identity{}, denied("session expired, reconnect")
	}
	return c.id, nil
}

func (h *hookClient) remember(id Identity) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache[id.ID] = cachedIdentity{id: id, exp: time.Now().Add(h.cacheL)}
	// Drop anything long expired so a busy server does not accumulate
	// identities for agents that never came back.
	now := time.Now()
	for k, v := range h.cache {
		if now.After(v.exp.Add(h.cacheL)) {
			delete(h.cache, k)
		}
	}
}

// forget drops a cached identity, used when the control plane disconnects it.
func (h *hookClient) forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.cache, id)
}

func (h *hookClient) AllowSubdomain(_ context.Context, id Identity, sub string, free bool) error {
	return ownReservations(slices.Contains(id.Subdomains, sub), "subdomain "+sub, free)
}

func (h *hookClient) AllowPort(_ context.Context, id Identity, port int, free bool) error {
	return ownReservations(slices.Contains(id.Ports, port), fmt.Sprintf("port %d", port), free)
}

// cmpOr returns a if it is non-empty, else b.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
