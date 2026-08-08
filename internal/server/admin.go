package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxAdminBody caps request bodies on the admin API.
const maxAdminBody = 64 << 10

// adminRoutes builds the admin API and the panel's static assets.
func (srv *Server) adminRoutes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /_api/login", srv.handleLogin)
	mux.HandleFunc("POST /_api/logout", srv.handleLogout)
	mux.HandleFunc("GET /_api/me", srv.handleMe)

	// Everything below requires a session.
	mux.Handle("GET /_api/overview", srv.requireAdmin(srv.handleOverview))
	mux.Handle("GET /_api/tokens", srv.requireAdmin(srv.handleListTokens))
	mux.Handle("POST /_api/tokens", srv.requireAdmin(srv.handleCreateToken))
	mux.Handle("PATCH /_api/tokens/{id}", srv.requireAdmin(srv.handleUpdateToken))
	mux.Handle("DELETE /_api/tokens/{id}", srv.requireAdmin(srv.handleDeleteToken))
	mux.Handle("POST /_api/tokens/{id}/rotate", srv.requireAdmin(srv.handleRotateToken))
	mux.Handle("GET /_api/tunnels", srv.requireAdmin(srv.handleListTunnels))
	mux.Handle("DELETE /_api/tunnels/{id}", srv.requireAdmin(srv.handleCloseTunnel))
	mux.Handle("GET /_api/sessions", srv.requireAdmin(srv.handleListSessions))
	mux.Handle("DELETE /_api/sessions/{id}", srv.requireAdmin(srv.handleCloseSession))

	mux.Handle("/_admin/", http.StripPrefix("/_admin", srv.adminUI()))
	mux.HandleFunc("GET /_admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_admin/", http.StatusFound)
	})

	return mux
}

// requireAdmin rejects unauthenticated calls.
func (srv *Server) requireAdmin(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(adminCookie)
		if err != nil || !srv.admin.valid(c.Value) {
			writeJSONError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		// SameSite=Strict already blocks cross-site form posts; requiring a
		// JSON content type on mutations closes the simple-request loophole
		// that a plain HTML form could otherwise squeeze through.
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeJSONError(w, http.StatusUnsupportedMediaType, "expected application/json")
				return
			}
		}
		next(w, r)
	})
}

func (srv *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := srv.admin.login(clientIP(r), body.Password)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, errLocked) {
			status = http.StatusTooManyRequests
		}
		srv.log.Warn("admin login failed", "ip", clientIP(r), "err", err)
		writeJSONError(w, status, err.Error())
		return
	}
	srv.log.Info("admin signed in", "ip", clientIP(r))
	srv.setAdminCookie(w, id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (srv *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil {
		srv.admin.logout(c.Value)
	}
	srv.clearAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe lets the panel decide whether to show the login screen.
func (srv *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(adminCookie)
	authed := err == nil && srv.admin.valid(c.Value)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": authed})
}

func (srv *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	srv.mu.Lock()
	sessions := len(srv.sessions)
	srv.mu.Unlock()

	var in, out int64
	for _, t := range srv.reg.List() {
		in += t.Traffic.In.Load()
		out += t.Traffic.Out.Load()
	}

	// The panel shows a ready-to-paste `burrow login`, which needs the port
	// agents dial rather than the address the listener happens to bind.
	controlPort := ""
	if _, port, err := net.SplitHostPort(srv.cfg.ControlAddr); err == nil {
		controlPort = port
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":      srv.version,
		"base_domain":  srv.cfg.BaseDomain,
		"public_host":  srv.cfg.PublicHost,
		"control_port": controlPort,
		"control_tls":  srv.cfg.TLSCert != "",
		"scheme":       srv.cfg.PublicScheme,
		"tcp_min":      srv.cfg.TCPPortMin,
		"tcp_max":      srv.cfg.TCPPortMax,
		"started_at":   srv.started.UTC(),
		"tunnels":      srv.reg.Count(),
		"sessions":     sessions,
		"tokens":       srv.store.Count(),
		"bytes_in":     in,
		"bytes_out":    out,
	})
}

// tokenView is a token as the panel sees it, with live session counts folded
// in.
type tokenView struct {
	Token
	ActiveSessions int `json:"active_sessions"`
	ActiveTunnels  int `json:"active_tunnels"`
}

func (srv *Server) handleListTokens(w http.ResponseWriter, _ *http.Request) {
	sessions, tunnels := srv.countByToken()

	tokens := srv.store.List()
	views := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		views = append(views, tokenView{
			Token:          t,
			ActiveSessions: sessions[t.ID],
			ActiveTunnels:  tunnels[t.ID],
		})
	}
	// Newest first: the token you just created is the one you want to copy.
	slices.SortFunc(views, func(a, b tokenView) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{"tokens": views})
}

func (srv *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in TokenInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	tok, err := srv.store.Create(in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	srv.log.Info("token created", "id", tok.ID, "name", tok.Name)
	writeJSON(w, http.StatusCreated, tok)
}

func (srv *Server) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	var in TokenInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	tok, err := srv.store.Update(id, in)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Disabling a token must take effect now, not at the agent's next
	// reconnect, or "disable" would be advisory only.
	if tok.Disabled {
		srv.disconnectToken(tok.ID, "token disabled")
	}
	srv.log.Info("token updated", "id", tok.ID, "name", tok.Name, "disabled", tok.Disabled)
	writeJSON(w, http.StatusOK, tok)
}

func (srv *Server) handleRotateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok, err := srv.store.Rotate(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	srv.disconnectToken(tok.ID, "token rotated")
	srv.log.Info("token rotated", "id", tok.ID, "name", tok.Name)
	writeJSON(w, http.StatusOK, tok)
}

func (srv *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := srv.store.Delete(id); err != nil {
		writeStoreError(w, err)
		return
	}
	srv.disconnectToken(id, "token deleted")
	srv.log.Info("token deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// tunnelView is one live tunnel as the panel sees it.
type tunnelView struct {
	ID        string    `json:"id"`
	Proto     string    `json:"proto"`
	Public    string    `json:"public"`
	Local     string    `json:"local"`
	Port      int       `json:"port,omitempty"`
	Subdomain string    `json:"subdomain,omitempty"`
	TokenID   string    `json:"token_id"`
	TokenName string    `json:"token_name"`
	SessionID string    `json:"session_id"`
	Hostname  string    `json:"hostname,omitempty"`
	AgentAddr string    `json:"agent_addr"`
	CreatedAt time.Time `json:"created_at"`
	Conns     int64     `json:"conns"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	LastSeen  int64     `json:"last_active_unix"`
}

func (srv *Server) handleListTunnels(w http.ResponseWriter, _ *http.Request) {
	tunnels := srv.reg.List()
	views := make([]tunnelView, 0, len(tunnels))
	for _, t := range tunnels {
		s := t.Session()
		views = append(views, tunnelView{
			ID:        t.ID,
			Proto:     t.Proto,
			Public:    t.Public(srv.cfg),
			Local:     t.LocalAddr,
			Port:      t.Port,
			Subdomain: t.Subdomain,
			TokenID:   s.TokenID,
			TokenName: s.TokenName,
			SessionID: s.ID,
			Hostname:  s.Hostname,
			AgentAddr: s.RemoteAddr,
			CreatedAt: t.Created.UTC(),
			Conns:     t.Conns.Load(),
			BytesIn:   t.Traffic.In.Load(),
			BytesOut:  t.Traffic.Out.Load(),
			LastSeen:  t.Traffic.LastActive.Load(),
		})
	}
	slices.SortFunc(views, func(a, b tunnelView) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{"tunnels": views})
}

func (srv *Server) handleCloseTunnel(w http.ResponseWriter, r *http.Request) {
	t := srv.reg.LookupID(r.PathValue("id"))
	if t == nil {
		writeJSONError(w, http.StatusNotFound, "no such tunnel")
		return
	}
	// Closing the agent session is the only honest way to close one tunnel:
	// the agent would otherwise keep believing it is published. It reconnects
	// and re-registers on its own, minus whatever an admin has revoked.
	t.Session().Close()
	srv.log.Info("tunnel closed by admin", "id", t.ID, "public", t.Public(srv.cfg))
	w.WriteHeader(http.StatusNoContent)
}

// sessionView is one connected agent as the panel sees it.
type sessionView struct {
	ID          string    `json:"id"`
	TokenID     string    `json:"token_id"`
	TokenName   string    `json:"token_name"`
	Hostname    string    `json:"hostname,omitempty"`
	AgentAddr   string    `json:"agent_addr"`
	ConnectedAt time.Time `json:"connected_at"`
	Tunnels     int       `json:"tunnels"`
}

func (srv *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	srv.mu.Lock()
	views := make([]sessionView, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		views = append(views, sessionView{
			ID:          s.ID,
			TokenID:     s.TokenID,
			TokenName:   s.TokenName,
			Hostname:    s.Hostname,
			AgentAddr:   s.RemoteAddr,
			ConnectedAt: s.Started.UTC(),
			Tunnels:     len(s.Tunnels()),
		})
	}
	srv.mu.Unlock()

	slices.SortFunc(views, func(a, b sessionView) int {
		return a.ConnectedAt.Compare(b.ConnectedAt)
	})
	writeJSON(w, http.StatusOK, map[string]any{"sessions": views})
}

func (srv *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	srv.mu.Lock()
	s := srv.sessions[id]
	srv.mu.Unlock()
	if s == nil {
		writeJSONError(w, http.StatusNotFound, "no such session")
		return
	}
	s.Close()
	srv.log.Info("session closed by admin", "id", id, "token", s.TokenName)
	w.WriteHeader(http.StatusNoContent)
}

// countByToken tallies live sessions and tunnels per token ID.
func (srv *Server) countByToken() (sessions, tunnels map[string]int) {
	sessions, tunnels = map[string]int{}, map[string]int{}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, s := range srv.sessions {
		sessions[s.TokenID]++
		tunnels[s.TokenID] += len(s.Tunnels())
	}
	return sessions, tunnels
}

// disconnectToken drops every agent authenticated with a token.
func (srv *Server) disconnectToken(tokenID, reason string) {
	srv.mu.Lock()
	var doomed []*Session
	for _, s := range srv.sessions {
		if s.TokenID == tokenID {
			doomed = append(doomed, s)
		}
	}
	srv.mu.Unlock()

	for _, s := range doomed {
		srv.log.Info("disconnecting agent", "session", s.ID, "token", s.TokenName, "reason", reason)
		s.Close()
	}
}

// decodeJSON reads a bounded JSON body and rejects unknown fields so that a
// typo in the panel surfaces as an error instead of a silent no-op.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeStoreError maps store failures onto HTTP status codes.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNoSuchToken) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSONError(w, http.StatusBadRequest, err.Error())
}
