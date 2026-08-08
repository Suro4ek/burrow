package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// adminCookie holds the opaque session id.
	adminCookie = "burrowd_admin"
	// adminSessionTTL is how long a login lasts without activity.
	adminSessionTTL = 12 * time.Hour
	// pbkdf2Iters is deliberately expensive: the panel is reachable from the
	// internet, so each guess should cost real time.
	pbkdf2Iters = 200_000
)

// adminAuth guards the admin API with a single operator password.
//
// The password is supplied at startup and never written to disk; only a salted
// PBKDF2 hash lives in memory, so a heap dump does not hand over the password.
type adminAuth struct {
	salt []byte
	hash []byte

	mu       sync.Mutex
	sessions map[string]time.Time // session id -> expiry
	// fails throttles online guessing, keyed by client IP.
	fails map[string]*failCounter
}

type failCounter struct {
	count int
	until time.Time
}

// errLocked is returned while a client is being throttled.
var errLocked = errors.New("too many failed attempts, try again later")

// newAdminAuth prepares the password verifier.
func newAdminAuth(password string) (*adminAuth, error) {
	if len(password) < 8 {
		return nil, errors.New("admin password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iters, 32)
	if err != nil {
		return nil, err
	}
	return &adminAuth{
		salt:     salt,
		hash:     hash,
		sessions: make(map[string]time.Time),
		fails:    make(map[string]*failCounter),
	}, nil
}

// login verifies a password and returns a new session id.
func (a *adminAuth) login(clientIP, password string) (string, error) {
	if err := a.checkThrottle(clientIP); err != nil {
		return "", err
	}

	candidate, err := pbkdf2.Key(sha256.New, password, a.salt, pbkdf2Iters, 32)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(candidate, a.hash) != 1 {
		a.recordFailure(clientIP)
		return "", errors.New("invalid password")
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)

	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, clientIP)
	a.sessions[id] = time.Now().Add(adminSessionTTL)
	a.sweepLocked()
	return id, nil
}

// checkThrottle rejects clients that have failed too often recently.
func (a *adminAuth) checkThrottle(clientIP string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.fails[clientIP]
	if ok && f.count >= 5 && time.Now().Before(f.until) {
		return errLocked
	}
	return nil
}

// recordFailure counts a bad password and extends the lockout window.
func (a *adminAuth) recordFailure(clientIP string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.fails[clientIP]
	if !ok || time.Now().After(f.until) {
		f = &failCounter{}
		a.fails[clientIP] = f
	}
	f.count++
	f.until = time.Now().Add(time.Duration(f.count) * time.Minute)
}

// valid reports whether a session id is live, refreshing its expiry.
func (a *adminAuth) valid(id string) bool {
	if id == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[id]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, id)
		return false
	}
	a.sessions[id] = time.Now().Add(adminSessionTTL)
	return true
}

// logout invalidates one session.
func (a *adminAuth) logout(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, id)
}

// sweepLocked drops expired sessions. Callers hold the lock.
func (a *adminAuth) sweepLocked() {
	now := time.Now()
	for id, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, id)
		}
	}
}

// setCookie issues the session cookie.
//
// Secure is set whenever the deployment is https, which is also what makes the
// __Host- style protections meaningful; SameSite=Strict is the CSRF defence
// for the mutating endpoints.
func (srv *Server) setAdminCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   srv.cfg.PublicScheme == "https",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(adminSessionTTL / time.Second),
	})
}

// clearAdminCookie expires the session cookie.
func (srv *Server) clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   srv.cfg.PublicScheme == "https",
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// clientIP extracts the caller's address for throttling. X-Forwarded-For is
// honoured because the panel normally sits behind Caddy on loopback.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	return hostOnly(r.RemoteAddr)
}
