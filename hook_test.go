package burrow_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suro4ek/burrow/internal/client"
	"github.com/suro4ek/burrow/internal/proto"
	"github.com/suro4ek/burrow/internal/server"
)

// controlPlane is a stand-in for the hosted service: it decides who may
// connect and collects the usage reports.
type controlPlane struct {
	*httptest.Server

	mu         sync.Mutex
	grant      server.HookAuthResponse
	authCalls  []server.HookAuthRequest
	reports    []server.HookUsageRequest
	disconnect []string
	authStatus int
}

func newControlPlane(t *testing.T, grant server.HookAuthResponse) *controlPlane {
	t.Helper()
	cp := &controlPlane{grant: grant, authStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth", func(w http.ResponseWriter, r *http.Request) {
		var req server.HookAuthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("auth hook got undecodable body: %v", err)
		}
		cp.mu.Lock()
		cp.authCalls = append(cp.authCalls, req)
		status, grant := cp.authStatus, cp.grant
		cp.mu.Unlock()

		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(grant)
	})
	mux.HandleFunc("POST /usage", func(w http.ResponseWriter, r *http.Request) {
		var req server.HookUsageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("usage hook got undecodable body: %v", err)
		}
		cp.mu.Lock()
		cp.reports = append(cp.reports, req)
		resp := server.HookUsageResponse{Disconnect: cp.disconnect}
		cp.disconnect = nil
		cp.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	cp.Server = httptest.NewServer(mux)
	t.Cleanup(cp.Close)
	return cp
}

func (cp *controlPlane) auths() []server.HookAuthRequest {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]server.HookAuthRequest(nil), cp.authCalls...)
}

func (cp *controlPlane) usage() []server.HookUsageRequest {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]server.HookUsageRequest(nil), cp.reports...)
}

func (cp *controlPlane) askDisconnect(id string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.disconnect = append(cp.disconnect, id)
}

// startHookServer boots a server that delegates authentication to cp.
func startHookServer(t *testing.T, cp *controlPlane, usage bool) *server.Server {
	t.Helper()

	cfg := server.DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.BaseDomain = baseDomain
	cfg.TCPBind = "127.0.0.1"
	cfg.TCPPortMin, cfg.TCPPortMax = 43000, 43500
	// The tokens file still exists but must be irrelevant: nothing in it can
	// grant access once an auth hook is configured.
	cfg.TokensFile = filepath.Join(t.TempDir(), "tokens.json")
	cfg.AuthHookURL = cp.URL + "/auth"
	if usage {
		cfg.UsageHookURL = cp.URL + "/usage"
		cfg.UsageInterval = 300 * time.Millisecond
	}

	srv, err := server.New(cfg, testLogger(t), "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	return srv
}

func TestHookAuthenticatesAgents(t *testing.T) {
	origin := httpOrigin(t)
	cp := newControlPlane(t, server.HookAuthResponse{
		OK: true, ID: "acct_42", Name: "acme", Subdomains: []string{"shop"},
	})
	srv := startHookServer(t, cp, false)

	granted := startAgent(t, srv, client.TunnelSpec{
		Proto: proto.ProtoHTTP, LocalAddr: origin, Subdomain: "shop",
	})
	if want := "http://shop." + baseDomain; granted[0].URL != want {
		t.Fatalf("URL = %q, want %q", granted[0].URL, want)
	}

	calls := cp.auths()
	if len(calls) == 0 {
		t.Fatal("the auth hook was never called")
	}
	if calls[0].Token != testToken {
		t.Errorf("hook saw token %q, want the agent's", calls[0].Token)
	}
	if calls[0].ServerVersion == "" || calls[0].RemoteAddr == "" {
		t.Errorf("hook request lacks context: %+v", calls[0])
	}

	req, _ := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/", nil)
	req.Host = "shop." + baseDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("tunnel status = %d", resp.StatusCode)
	}
}

// TestHookRejectionReachesTheAgent covers a clean "no" from the control plane:
// the reason must arrive verbatim and the agent must stop instead of looping.
func TestHookRejectionReachesTheAgent(t *testing.T) {
	cp := newControlPlane(t, server.HookAuthResponse{
		OK: false, Error: "subscription expired",
	})
	srv := startHookServer(t, cp, false)

	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		Tunnels:    []client.TunnelSpec{{Proto: proto.ProtoHTTP, LocalAddr: "127.0.0.1:1"}},
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = c.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "subscription expired") {
		t.Fatalf("got %v, want the control plane's reason", err)
	}
}

// TestHookOutageIsNotReportedAsBadCredentials guards a distinction that
// matters operationally: a broken control plane must not tell users their
// token is invalid.
func TestHookOutageIsNotReportedAsBadCredentials(t *testing.T) {
	cp := newControlPlane(t, server.HookAuthResponse{OK: true, ID: "acct_1"})
	cp.mu.Lock()
	cp.authStatus = http.StatusInternalServerError
	cp.mu.Unlock()

	srv := startHookServer(t, cp, false)

	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		Tunnels:    []client.TunnelSpec{{Proto: proto.ProtoHTTP, LocalAddr: "127.0.0.1:1"}},
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	err = c.Run(ctx)

	// Retrying is right here — the outage is transient — so Run returns nil on
	// context expiry rather than a rejection.
	if err != nil && strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("an outage was reported as bad credentials: %v", err)
	}
}

func TestHookReportsUsageAndHonoursDisconnect(t *testing.T) {
	origin := httpOrigin(t)
	cp := newControlPlane(t, server.HookAuthResponse{OK: true, ID: "acct_9", Name: "billed"})
	srv := startHookServer(t, cp, true)

	startAgent(t, srv, client.TunnelSpec{
		Proto: proto.ProtoHTTP, LocalAddr: origin, Subdomain: "metered",
	})

	// Push some traffic so the counters are not all zero.
	for range 3 {
		req, _ := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/", nil)
		req.Host = "metered." + baseDomain
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	var seen server.HookUsageTunnel
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, rep := range cp.usage() {
			for _, tun := range rep.Tunnels {
				if tun.TokenID == "acct_9" && tun.BytesOut > 0 {
					seen = tun
				}
			}
		}
		if seen.TunnelID != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if seen.TunnelID == "" {
		t.Fatal("no usage was reported for the tunnel")
	}
	if seen.Conns == 0 || seen.BytesIn == 0 {
		t.Errorf("counters look wrong: %+v", seen)
	}
	if seen.Public != "http://metered."+baseDomain {
		t.Errorf("public address = %q", seen.Public)
	}

	// Now have the control plane cut the account off, as it would for an
	// exhausted quota, and check the tunnel actually goes away.
	cp.askDisconnect("acct_9")
	gone := false
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/", nil)
		req.Host = "metered." + baseDomain
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				gone = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !gone {
		t.Error("the tunnel survived a disconnect instruction from the control plane")
	}
}

// TestHookReportsClosedTunnels covers the short-lived tunnel case: one that
// opens and closes inside a single reporting interval must still be billed.
func TestHookReportsClosedTunnels(t *testing.T) {
	origin := httpOrigin(t)
	cp := newControlPlane(t, server.HookAuthResponse{OK: true, ID: "acct_brief"})
	srv := startHookServer(t, cp, true)

	stop := startAgentStoppable(t, srv, client.TunnelSpec{
		Proto: proto.ProtoHTTP, LocalAddr: origin, Subdomain: "brief",
	})
	stop()

	found := false
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !found {
		for _, rep := range cp.usage() {
			for _, tun := range rep.Tunnels {
				if tun.TokenID == "acct_brief" && tun.Closed {
					found = true
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !found {
		t.Error("a tunnel that closed between reports was never accounted for")
	}
}

func TestUsageHookRequiresAuthHook(t *testing.T) {
	cfg := server.DefaultConfig()
	cfg.BaseDomain = baseDomain
	cfg.TokensFile = filepath.Join(t.TempDir(), "tokens.json")
	cfg.UsageHookURL = "https://example.com/usage"

	if _, err := server.New(cfg, testLogger(t), "test"); err == nil {
		t.Fatal("usage reporting without an auth hook should be refused")
	}
}

// startAgentStoppable connects an agent and hands back a way to stop it, so a
// test can observe what happens when a tunnel goes away.
func startAgentStoppable(t *testing.T, srv *server.Server, specs ...client.TunnelSpec) func() {
	t.Helper()

	ready := make(chan []proto.TunnelResp, 1)
	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		Tunnels:    specs,
		Log:        testLogger(t),
		OnReady: func(g []proto.TunnelResp) {
			select {
			case ready <- g:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		stop()
		t.Fatal("agent never connected")
	}
	return stop
}
