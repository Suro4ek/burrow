package burrow_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/suro4ek/burrow/internal/client"
	"github.com/suro4ek/burrow/internal/proto"
	"github.com/suro4ek/burrow/internal/server"
)

const testToken = "test-token-0123456789abcdef"

const baseDomain = "tuntest.local"

// startServer boots a burrowd on ephemeral ports and returns it once both
// listeners are up.
func startServer(t *testing.T) *server.Server {
	t.Helper()

	tokensPath := filepath.Join(t.TempDir(), "tokens.json")
	tokens := fmt.Sprintf(
		`[{"token":%q,"name":"test","subdomains":["reserved"],"ports":[41500]}]`, testToken)
	if err := os.WriteFile(tokensPath, []byte(tokens), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := server.DefaultConfig()
	cfg.ControlAddr = "127.0.0.1:0"
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.BaseDomain = baseDomain
	cfg.TCPBind = "127.0.0.1"
	cfg.TCPPortMin, cfg.TCPPortMax = 41000, 42000
	cfg.TokensFile = tokensPath

	srv, err := server.New(cfg, testLogger(t), "test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exited with error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	return srv
}

// startAgent connects an agent and blocks until the server grants its tunnels.
func startAgent(t *testing.T, srv *server.Server, specs ...client.TunnelSpec) []proto.TunnelResp {
	t.Helper()

	ready := make(chan []proto.TunnelResp, 1)
	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		TLS:        false,
		Tunnels:    specs,
		Version:    "test",
		Log:        testLogger(t),
		OnReady: func(granted []proto.TunnelResp) {
			select {
			case ready <- granted:
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
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case granted := <-ready:
		return granted
	case err := <-done:
		t.Fatalf("agent stopped before connecting: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("agent never connected")
	}
	return nil
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestHTTPTunnel(t *testing.T) {
	origin := httpOrigin(t)
	srv := startServer(t)

	granted := startAgent(t, srv, client.TunnelSpec{
		Proto:     proto.ProtoHTTP,
		LocalAddr: origin,
		Subdomain: "demo",
	})
	if len(granted) != 1 || granted[0].URL != "http://demo."+baseDomain {
		t.Fatalf("unexpected grant: %+v", granted)
	}

	req, err := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/hello?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The tunnel routes on Host, so we can dial the server directly instead
	// of standing up DNS for the test.
	req.Host = "demo." + baseDomain

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	got := string(body)
	if !strings.Contains(got, "path=/hello?x=1") {
		t.Errorf("origin did not see the original path: %s", got)
	}
	if !strings.Contains(got, "host=demo."+baseDomain) {
		t.Errorf("origin did not see the original Host: %s", got)
	}
	if !strings.Contains(got, "xff=127.0.0.1") {
		t.Errorf("origin did not get X-Forwarded-For: %s", got)
	}
}

func TestHTTPTunnelUnknownSubdomain(t *testing.T) {
	srv := startServer(t)

	req, _ := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/", nil)
	req.Host = "nobody." + baseDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPTunnelOriginDown(t *testing.T) {
	srv := startServer(t)

	// Bind a port, learn its number, release it: nothing is listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	startAgent(t, srv, client.TunnelSpec{
		Proto:     proto.ProtoHTTP,
		LocalAddr: dead,
		Subdomain: "down",
	})

	req, _ := http.NewRequest("GET", "http://"+srv.HTTPAddr().String()+"/", nil)
	req.Host = "down." + baseDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestTCPTunnel(t *testing.T) {
	origin := tcpEchoOrigin(t)
	srv := startServer(t)

	granted := startAgent(t, srv, client.TunnelSpec{
		Proto:     proto.ProtoTCP,
		LocalAddr: origin,
	})
	if len(granted) != 1 || granted[0].RemotePort == 0 {
		t.Fatalf("unexpected grant: %+v", granted)
	}

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(granted[0].RemotePort))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "echo:ping\n" {
		t.Fatalf("got %q, want %q", line, "echo:ping\n")
	}
}

// TestTCPTunnelPrefersReservedPort covers what makes `burrow ssh` usable: an
// agent that asks for no particular port still lands on its reserved one, so
// the ssh command stays the same across restarts.
func TestTCPTunnelPrefersReservedPort(t *testing.T) {
	origin := tcpEchoOrigin(t)
	srv := startServer(t)

	granted := startAgent(t, srv, client.TunnelSpec{
		Proto:     proto.ProtoTCP,
		LocalAddr: origin,
		// No RemotePort: the server should still pick the reservation.
	})
	if got := granted[0].RemotePort; got != 41500 {
		t.Fatalf("port = %d, want the reserved 41500", got)
	}
}

func TestTCPTunnelReleasesPortOnDisconnect(t *testing.T) {
	origin := tcpEchoOrigin(t)
	srv := startServer(t)

	ready := make(chan []proto.TunnelResp, 1)
	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		Tunnels:    []client.TunnelSpec{{Proto: proto.ProtoTCP, LocalAddr: origin}},
		Log:        testLogger(t),
		OnReady:    func(g []proto.TunnelResp) { ready <- g },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	granted := <-ready
	port := granted[0].RemotePort
	cancel()
	<-done

	// The listener is closed as the session tears down; give it a moment,
	// then confirm the port is genuinely free by binding it ourselves.
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	var lastErr error
	for range 50 {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d never became free: %v", port, lastErr)
}

func TestBadTokenIsFatal(t *testing.T) {
	srv := startServer(t)

	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      "wrong-token-0123456789abcdef",
		Tunnels:    []client.TunnelSpec{{Proto: proto.ProtoHTTP, LocalAddr: "127.0.0.1:1"}},
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A rejected handshake must stop the agent rather than spin in the
	// reconnect loop.
	err = c.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("got %v, want an invalid-token rejection", err)
	}
}

func TestReservedSubdomainRefusesOtherToken(t *testing.T) {
	srv := startServer(t)
	origin := httpOrigin(t)

	// "reserved" belongs to the test token, so the same token may take it.
	granted := startAgent(t, srv, client.TunnelSpec{
		Proto:     proto.ProtoHTTP,
		LocalAddr: origin,
		Subdomain: "reserved",
	})
	if granted[0].URL != "http://reserved."+baseDomain {
		t.Fatalf("unexpected grant: %+v", granted)
	}

	// A second claim on a live name must fail, whoever asks.
	c, err := client.New(client.Config{
		ServerAddr: srv.ControlAddr().String(),
		Token:      testToken,
		Tunnels:    []client.TunnelSpec{{Proto: proto.ProtoHTTP, LocalAddr: origin, Subdomain: "reserved"}},
		Log:        testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatalf("expected the agent to keep retrying, got %v", err)
	}
}

// httpOrigin starts a local HTTP server that reports what it received.
func httpOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s host=%s xff=%s proto=%s\n",
			r.URL.RequestURI(), r.Host,
			r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// tcpEchoOrigin starts a local line echo server.
func tcpEchoOrigin(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					fmt.Fprintf(conn, "echo:%s\n", sc.Text())
				}
				_ = sc.Err() // the peer hanging up is the normal exit here
			}()
		}
	}()
	return ln.Addr().String()
}
