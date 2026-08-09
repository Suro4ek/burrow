package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// start brings up a server and returns the address to dial.
func start(t *testing.T, s *Server) (string, *Server) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the agent's ssh server needs a pty, which Windows does not provide")
	}
	if s.Log == nil {
		s.Log = quietLogger()
	}
	srv, err := New(filepath.Join(t.TempDir(), "host_key"), s)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); srv.Close() })
	go srv.Serve(ln)
	return ln.Addr().String(), srv
}

// dial connects as a client would.
func dial(t *testing.T, addr string, auth ...xssh.AuthMethod) (*xssh.Client, error) {
	t.Helper()
	return xssh.Dial("tcp", addr, &xssh.ClientConfig{
		User:            "tester",
		Auth:            auth,
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
}

func run(t *testing.T, c *xssh.Client, cmd string) string {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output(cmd)
	if err != nil {
		t.Fatalf("running %q: %v", cmd, err)
	}
	return strings.TrimSpace(string(out))
}

// TestServesTheGivenDirectory is the whole point of the command: a shell that
// starts where it was launched, without a system sshd anywhere.
func TestServesTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("found me"), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, _ := start(t, &Server{Dir: dir, Password: "secret-password"})
	c, err := dial(t, addr, xssh.Password("secret-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// macOS puts temp dirs under /private, which the shell resolves.
	if got := run(t, c, "pwd"); !strings.HasSuffix(got, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
	if got := run(t, c, "cat marker.txt"); got != "found me" {
		t.Errorf("cat = %q", got)
	}
}

func TestPasswordAuth(t *testing.T) {
	addr, _ := start(t, &Server{Dir: t.TempDir(), Password: "right-password"})

	if _, err := dial(t, addr, xssh.Password("wrong-password")); err == nil {
		t.Error("the wrong password was accepted")
	}
	c, err := dial(t, addr, xssh.Password("right-password"))
	if err != nil {
		t.Fatalf("the right password was rejected: %v", err)
	}
	c.Close()
}

// TestPasswordAuthOffByDefault guards the case where only keys should work: an
// empty password must refuse everything rather than accept everything.
func TestPasswordAuthOffByDefault(t *testing.T) {
	addr, _ := start(t, &Server{Dir: t.TempDir()})
	if _, err := dial(t, addr, xssh.Password("")); err == nil {
		t.Error("a server with no password accepted an empty one")
	}
}

func TestPublicKeyAuth(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	authorized := strings.TrimSpace(string(xssh.MarshalAuthorizedKey(sshPub)))

	addr, _ := start(t, &Server{Dir: t.TempDir(), AuthorizedKeys: []string{authorized}})
	c, err := dial(t, addr, xssh.PublicKeys(signer))
	if err != nil {
		t.Fatalf("an authorized key was rejected: %v", err)
	}
	c.Close()

	// A different key must not get in.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := xssh.NewSignerFromKey(other)
	if _, err := dial(t, addr, xssh.PublicKeys(otherSigner)); err == nil {
		t.Error("an unauthorized key was accepted")
	}
}

// TestHostKeyIsStable is what makes step 3 of the CLI output worth printing: a
// key regenerated per run would make ssh refuse to reconnect, loudly, after
// known_hosts had learned the first one.
func TestHostKeyIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")

	first, err := New(path, &Server{Dir: t.TempDir(), Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(path, &Server{Dir: t.TempDir(), Log: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("fingerprint changed between runs: %s then %s",
			first.Fingerprint(), second.Fingerprint())
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("host key mode = %o, want 600", perm)
	}

	line := first.HostKeyLine("tun.example.com", 25343)
	if !strings.HasPrefix(line, "[tun.example.com]:25343 ssh-ed25519 ") {
		t.Errorf("known_hosts line looks wrong: %q", line)
	}
}

// TestListensOnLoopbackOnly: the tunnel is the only way in, so the port must
// not be reachable from the network even while the server is running.
func TestListensOnLoopbackOnly(t *testing.T) {
	addr, _ := start(t, &Server{Dir: t.TempDir(), Password: "x"})
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Errorf("listening on %s, want 127.0.0.1", host)
	}
}

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		p := NewPassword()
		if len(p) < 20 {
			t.Fatalf("password %q is too short to be worth generating", p)
		}
		if seen[p] {
			t.Fatal("NewPassword repeated itself")
		}
		seen[p] = true
	}
}
