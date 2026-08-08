package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

const testAdminPassword = "correct horse battery"

// newAdminServer builds a server with the panel enabled but nothing listening.
func newAdminServer(t *testing.T) *Server {
	t.Helper()

	cfg := DefaultConfig()
	cfg.BaseDomain = "tuntest.local"
	cfg.TokensFile = filepath.Join(t.TempDir(), "tokens.json")
	cfg.AdminPassword = testAdminPassword

	srv, err := New(cfg, slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// call issues a request against the admin handler, carrying a cookie if given.
func call(t *testing.T, srv *Server, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Host = "tuntest.local"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// signIn logs in and returns the session cookie.
func signIn(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	w := call(t, srv, "POST", "/_api/login", map[string]string{"password": testAdminPassword}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookie {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

func TestAdminRequiresAuth(t *testing.T) {
	srv := newAdminServer(t)

	for _, path := range []string{"/_api/overview", "/_api/tokens", "/_api/tunnels", "/_api/sessions"} {
		w := call(t, srv, "GET", path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, w.Code)
		}
	}

	w := call(t, srv, "POST", "/_api/tokens", map[string]string{"name": "x"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /_api/tokens without a session = %d, want 401", w.Code)
	}
}

func TestAdminRejectsWrongPassword(t *testing.T) {
	srv := newAdminServer(t)

	w := call(t, srv, "POST", "/_api/login", map[string]string{"password": "guess"}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login with a wrong password = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookie && c.Value != "" {
			t.Fatal("a failed login handed out a session cookie")
		}
	}
}

func TestAdminSessionCookieIsHardened(t *testing.T) {
	srv := newAdminServer(t)
	c := signIn(t, srv)

	if !c.HttpOnly {
		t.Error("session cookie is readable from JavaScript")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("session cookie is not SameSite=Strict, so it is exposed to CSRF")
	}
}

func TestAdminTokenLifecycle(t *testing.T) {
	srv := newAdminServer(t)
	c := signIn(t, srv)

	w := call(t, srv, "POST", "/_api/tokens", map[string]any{
		"name":       "laptop",
		"subdomains": []string{"dev"},
		"ports":      []int{25343},
	}, c)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	created := decode[Token](t, w)
	if created.Secret == "" || created.ID == "" {
		t.Fatalf("create returned an incomplete token: %+v", created)
	}

	w = call(t, srv, "GET", "/_api/tokens", nil, c)
	list := decode[struct {
		Tokens []tokenView `json:"tokens"`
	}](t, w)
	if len(list.Tokens) != 1 || list.Tokens[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Tokens)
	}

	w = call(t, srv, "PATCH", "/_api/tokens/"+created.ID, map[string]any{"disabled": true}, c)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body.String())
	}
	if !decode[Token](t, w).Disabled {
		t.Error("patch did not disable the token")
	}

	w = call(t, srv, "POST", "/_api/tokens/"+created.ID+"/rotate", map[string]any{}, c)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", w.Code, w.Body.String())
	}
	if decode[Token](t, w).Secret == created.Secret {
		t.Error("rotate kept the old secret")
	}

	w = call(t, srv, "DELETE", "/_api/tokens/"+created.ID, nil, c)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	if srv.store.Count() != 0 {
		t.Error("token survived deletion")
	}
}

func TestAdminRejectsUnknownTokenID(t *testing.T) {
	srv := newAdminServer(t)
	c := signIn(t, srv)

	w := call(t, srv, "DELETE", "/_api/tokens/nope", nil, c)
	if w.Code != http.StatusNotFound {
		t.Errorf("delete of a missing token = %d, want 404", w.Code)
	}
}

func TestAdminLogoutEndsTheSession(t *testing.T) {
	srv := newAdminServer(t)
	c := signIn(t, srv)

	if w := call(t, srv, "POST", "/_api/logout", map[string]any{}, c); w.Code != http.StatusOK {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := call(t, srv, "GET", "/_api/tokens", nil, c); w.Code != http.StatusUnauthorized {
		t.Error("the session still works after logout")
	}
}

// TestAdminDisabledWithoutPassword makes sure the panel is not reachable when
// no password was configured.
func TestAdminDisabledWithoutPassword(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BaseDomain = "tuntest.local"
	cfg.TokensFile = filepath.Join(t.TempDir(), "tokens.json")

	srv, err := New(cfg, slog.New(slog.DiscardHandler), "test")
	if err != nil {
		t.Fatal(err)
	}
	if srv.AdminEnabled() {
		t.Fatal("the panel is enabled without a password")
	}

	// With the panel off, /_api must not be handled at all: it falls through
	// to the ordinary base-domain page.
	w := call(t, srv, "POST", "/_api/login", map[string]string{"password": "x"}, nil)
	if w.Code == http.StatusOK && bytes.Contains(w.Body.Bytes(), []byte("ok")) {
		t.Error("login answered even though the panel is disabled")
	}
}

// TestAdminPanelIsNotServedOnTunnelSubdomains guards the routing rule that
// keeps the panel on the bare base domain.
func TestAdminPanelIsNotServedOnTunnelSubdomains(t *testing.T) {
	srv := newAdminServer(t)

	req := httptest.NewRequest("GET", "/_api/overview", nil)
	req.Host = "someone.tuntest.local"
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	// No tunnel is registered for that name, so this must be a tunnel 404 and
	// never the admin API.
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("not signed in")) {
		t.Error("the admin API answered on a tunnel subdomain")
	}
}

func TestAdminMutationRequiresJSONContentType(t *testing.T) {
	srv := newAdminServer(t)
	c := signIn(t, srv)

	// A cross-site HTML form can only send simple content types; refusing them
	// is what stops such a form from creating tokens.
	req := httptest.NewRequest("POST", "/_api/tokens", bytes.NewReader([]byte(`{"name":"x"}`)))
	req.Host = "tuntest.local"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
}
