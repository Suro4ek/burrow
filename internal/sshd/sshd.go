// Package sshd is the SSH server the agent runs itself.
//
// `burrow ssh` does not forward to a system sshd: it starts this, which hands
// out a shell in the directory the command was run from, as the user who ran
// it. That means remote access to a machine with no sshd configured, no port
// opened and no root involved.
//
// It only ever listens on loopback. Reaching it means coming through the
// tunnel, so the exposure is exactly the tunnel's lifetime and nothing is
// listening on the machine once the agent stops.
package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// Server serves shells over SSH.
type Server struct {
	// Dir is what a session starts in.
	Dir string
	// Password authenticates a session. Empty disables password auth.
	Password string
	// AuthorizedKeys may connect without a password.
	AuthorizedKeys []string
	// Log receives diagnostics.
	Log *slog.Logger

	hostKey xssh.Signer
	parsed  []gssh.PublicKey
	srv     *gssh.Server

	mu       sync.Mutex
	sessions int
}

// New prepares a server, loading or creating the persistent host key.
func New(hostKeyPath string, s *Server) (*Server, error) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	signer, err := loadOrCreateHostKey(hostKeyPath)
	if err != nil {
		return nil, err
	}
	s.hostKey = signer

	for _, line := range s.AuthorizedKeys {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, _, _, _, err := gssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			s.Log.Warn("ignoring unparseable authorized key", "err", err)
			continue
		}
		s.parsed = append(s.parsed, key)
	}
	return s, nil
}

// HostKeyLine returns the known_hosts entry for this server at addr, which is
// what lets someone connect without being asked to confirm a fingerprint.
func (s *Server) HostKeyLine(host string, port int) string {
	pub := s.hostKey.PublicKey()
	return fmt.Sprintf("[%s]:%d %s %s", host, port, pub.Type(),
		base64.StdEncoding.EncodeToString(pub.Marshal()))
}

// Fingerprint is the SHA256 fingerprint ssh prints on first connection.
func (s *Server) Fingerprint() string {
	return xssh.FingerprintSHA256(s.hostKey.PublicKey())
}

// Listen opens the loopback listener the tunnel will point at.
func (s *Server) Listen() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

// Serve handles connections until ln closes.
func (s *Server) Serve(ln net.Listener) error {
	s.srv = &gssh.Server{
		Handler:          s.handleSession,
		PasswordHandler:  s.checkPassword,
		PublicKeyHandler: s.checkPublicKey,
	}
	s.srv.AddHostKey(s.hostKey)
	return s.srv.Serve(ln)
}

// Close stops the server and every live session.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// Sessions reports how many shells are currently open.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

func (s *Server) checkPassword(ctx gssh.Context, password string) bool {
	if s.Password == "" {
		return false
	}
	ok := subtle.ConstantTimeCompare([]byte(password), []byte(s.Password)) == 1
	if !ok {
		s.Log.Warn("ssh: wrong password", "from", ctx.RemoteAddr().String(), "user", ctx.User())
	}
	return ok
}

func (s *Server) checkPublicKey(ctx gssh.Context, key gssh.PublicKey) bool {
	for _, allowed := range s.parsed {
		if gssh.KeysEqual(key, allowed) {
			return true
		}
	}
	return false
}

// handleSession runs a shell, or a single command when one was requested.
func (s *Server) handleSession(sess gssh.Session) {
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sessions--
		s.mu.Unlock()
	}()

	s.Log.Info("ssh session opened",
		"from", sess.RemoteAddr().String(), "user", sess.User(), "dir", s.Dir)
	defer s.Log.Info("ssh session closed", "from", sess.RemoteAddr().String())

	shell := loginShell()
	var cmd *exec.Cmd
	if raw := sess.RawCommand(); raw != "" {
		// `ssh host <command>` and scp/rsync both arrive this way.
		cmd = exec.Command(shell, "-c", raw)
	} else {
		cmd = exec.Command(shell, "-l")
	}
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptyReq, winCh, isPty := sess.Pty()
	if !isPty {
		// No terminal was asked for: wire the pipes straight through, which is
		// what non-interactive callers expect.
		s.runPlain(sess, cmd)
		return
	}

	cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	f, err := pty.Start(cmd)
	if err != nil {
		s.Log.Error("ssh: cannot start a shell", "err", err)
		io.WriteString(sess.Stderr(), "burrow: cannot start a shell: "+err.Error()+"\n")
		_ = sess.Exit(1)
		return
	}
	defer f.Close()

	go func() {
		for win := range winCh {
			_ = pty.Setsize(f, &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)})
		}
	}()
	go func() { _, _ = io.Copy(f, sess) }() // client -> shell
	_, _ = io.Copy(sess, f)                 // shell -> client

	// The client hung up: make sure the shell does not outlive it.
	_ = cmd.Process.Signal(syscall.SIGHUP)
	err = cmd.Wait()
	_ = sess.Exit(exitCode(err))
}

// runPlain handles a session with no pty.
func (s *Server) runPlain(sess gssh.Session, cmd *exec.Cmd) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = sess.Exit(1)
		return
	}
	go func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, sess)
	}()
	cmd.Stdout = sess
	cmd.Stderr = sess.Stderr()

	if err := cmd.Start(); err != nil {
		io.WriteString(sess.Stderr(), "burrow: "+err.Error()+"\n")
		_ = sess.Exit(1)
		return
	}
	_ = sess.Exit(exitCode(cmd.Wait()))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

// loginShell picks the user's shell, falling back to something that exists.
func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		for _, candidate := range []string{"/bin/zsh", "/bin/bash"} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "/bin/sh"
}

// CurrentUser is the account a session will run as, for the connect command.
func CurrentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "user"
}

// NewPassword generates a session password.
//
// Six words from a small alphabet rather than a long random string: it has to
// survive being read aloud and retyped, and 4 groups of 5 characters from this
// alphabet is still ~90 bits.
func NewPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("sshd: crypto/rand unavailable: " + err.Error())
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%5 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return sb.String()
}

// loadOrCreateHostKey keeps the host key on disk.
//
// A key generated per run would change the fingerprint every time, and ssh
// would refuse to connect with a loud warning about a possible attack — after
// the first session had already taught known_hosts the old one.
func loadOrCreateHostKey(path string) (xssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		signer, err := xssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		return signer, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := xssh.MarshalPrivateKey(priv, "burrow host key")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(der), 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	return xssh.NewSignerFromKey(priv)
}
