package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/suro4ek/burrow/internal/netutil"
)

// Token is one agent's credentials and its allowances.
//
// Subdomains and Ports are *reservations*: names listed here belong to this
// token and no other token may claim them, even when the server otherwise
// hands out names freely (see Config.FreeSubdomains).
type Token struct {
	ID         string     `json:"id"`
	Secret     string     `json:"token"`
	Name       string     `json:"name"`
	Subdomains []string   `json:"subdomains,omitempty"`
	Ports      []int      `json:"ports,omitempty"`
	MaxTunnels int        `json:"max_tunnels,omitempty"`
	DenyTCP    bool       `json:"deny_tcp,omitempty"`
	SSHKeys    []string   `json:"ssh_keys,omitempty"`
	Disabled   bool       `json:"disabled,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
}

// clone returns a deep copy so callers can never mutate stored state through
// the slices they were handed.
func (t *Token) clone() Token {
	c := *t
	c.Subdomains = slices.Clone(t.Subdomains)
	c.Ports = slices.Clone(t.Ports)
	c.SSHKeys = slices.Clone(t.SSHKeys)
	if t.LastSeen != nil {
		seen := *t.LastSeen
		c.LastSeen = &seen
	}
	return c
}

// TokenInput carries the mutable fields of a token. Nil pointers mean "leave
// this field alone", which is what lets one type serve both create and patch.
type TokenInput struct {
	Name       *string   `json:"name,omitempty"`
	Subdomains *[]string `json:"subdomains,omitempty"`
	Ports      *[]int    `json:"ports,omitempty"`
	MaxTunnels *int      `json:"max_tunnels,omitempty"`
	DenyTCP    *bool     `json:"deny_tcp,omitempty"`
	Disabled   *bool     `json:"disabled,omitempty"`
	SSHKeys    *[]string `json:"ssh_keys,omitempty"`
}

// Store holds the token list and persists it as JSON.
//
// Every mutation rewrites the whole file through a temp file and a rename, so
// a crash mid-write cannot leave a truncated token list behind: either the old
// file or the new one is on disk, never a mix.
type Store struct {
	path string

	mu       sync.RWMutex
	tokens   []*Token
	byID     map[string]*Token
	bySecret map[string]*Token
}

// OpenStore loads the token file, creating an empty one if it does not exist.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.tokens = nil
		s.reindex()
		if err := s.persist(); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("read tokens file: %w", err)
	}

	var list []*Token
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("parse tokens file %s: %w", path, err)
	}

	migrated := false
	for i, t := range list {
		if t == nil || strings.TrimSpace(t.Secret) == "" {
			return nil, fmt.Errorf("token #%d: empty token value", i)
		}
		// Files written before the admin panel existed have no id or
		// timestamp; fill them in and save the upgrade back.
		if t.ID == "" {
			t.ID = netutil.RandID(10)
			migrated = true
		}
		if t.Name == "" {
			t.Name = fmt.Sprintf("token-%d", i+1)
			migrated = true
		}
		if t.CreatedAt.IsZero() {
			t.CreatedAt = time.Now().UTC()
			migrated = true
		}
		for j, sub := range t.Subdomains {
			t.Subdomains[j] = strings.ToLower(strings.TrimSpace(sub))
		}
	}

	s.tokens = list
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("tokens file %s: %w", path, err)
	}
	s.reindex()

	if migrated {
		if err := s.persist(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// validate checks the whole list for conflicts. The caller holds no lock
// during load; every other caller holds the write lock.
func (s *Store) validate() error {
	secrets := make(map[string]bool, len(s.tokens))
	ids := make(map[string]bool, len(s.tokens))
	subOwner := make(map[string]string)
	portOwner := make(map[int]string)

	for _, t := range s.tokens {
		if len(t.Secret) < 16 {
			return fmt.Errorf("token %q: secret too short, use at least 16 characters", t.Name)
		}
		if secrets[t.Secret] {
			return fmt.Errorf("token %q: duplicate secret", t.Name)
		}
		if ids[t.ID] {
			return fmt.Errorf("token %q: duplicate id %s", t.Name, t.ID)
		}
		secrets[t.Secret], ids[t.ID] = true, true

		for _, sub := range t.Subdomains {
			if !validSubdomain(sub) {
				return fmt.Errorf("token %q: invalid reserved subdomain %q", t.Name, sub)
			}
			if reservedSubdomains[sub] {
				return fmt.Errorf("token %q: subdomain %q is reserved by the server", t.Name, sub)
			}
			if owner, taken := subOwner[sub]; taken && owner != t.ID {
				return fmt.Errorf("subdomain %q reserved twice", sub)
			}
			subOwner[sub] = t.ID
		}
		for _, p := range t.Ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("token %q: invalid reserved port %d", t.Name, p)
			}
			if owner, taken := portOwner[p]; taken && owner != t.ID {
				return fmt.Errorf("port %d reserved twice", p)
			}
			portOwner[p] = t.ID
		}
	}
	return nil
}

// reindex rebuilds the lookup maps. Callers hold the write lock.
func (s *Store) reindex() {
	s.byID = make(map[string]*Token, len(s.tokens))
	s.bySecret = make(map[string]*Token, len(s.tokens))
	for _, t := range s.tokens {
		s.byID[t.ID] = t
		s.bySecret[t.Secret] = t
	}
}

// persist writes the token list atomically. Callers hold the write lock.
func (s *Store) persist() error {
	b, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tokens: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".tokens-*.json")
	if err != nil {
		return fmt.Errorf("create temp tokens file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp tokens file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp tokens file: %w", err)
	}
	// fsync before rename: rename is atomic with respect to readers, but
	// without the sync a power loss can leave the new name pointing at
	// unwritten blocks.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp tokens file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp tokens file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace tokens file: %w", err)
	}
	return nil
}

// Lookup resolves an agent secret.
//
// This is a plain map lookup rather than a constant-time comparison: tokens
// are high-entropy secrets, so there is no low-entropy prefix for a timing
// oracle to walk. Do not reuse this pattern for user-chosen passwords.
func (s *Store) Lookup(secret string) (Token, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.bySecret[secret]
	if !ok {
		return Token{}, false
	}
	return t.clone(), true
}

// Get returns a token by ID.
func (s *Store) Get(id string) (Token, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[id]
	if !ok {
		return Token{}, false
	}
	return t.clone(), true
}

// List returns every token, newest last.
func (s *Store) List() []Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Token, 0, len(s.tokens))
	for _, t := range s.tokens {
		out = append(out, t.clone())
	}
	return out
}

// Count returns the number of tokens.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// Create adds a token with a freshly generated secret.
func (s *Store) Create(in TokenInput) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := &Token{
		ID:        netutil.RandID(10),
		Secret:    NewSecret(),
		Name:      "unnamed",
		CreatedAt: time.Now().UTC(),
	}
	applyInput(t, in)
	if strings.TrimSpace(t.Name) == "" {
		return Token{}, errors.New("name is required")
	}

	// Append first so commit validates the list as it would be, then undo the
	// append if it refuses: a rejected create must leave no trace.
	s.tokens = append(s.tokens, t)
	if err := s.commit(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		s.reindex()
		return Token{}, err
	}
	return t.clone(), nil
}

// Update applies a partial change to a token.
func (s *Store) Update(id string, in TokenInput) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.byID[id]
	if !ok {
		return Token{}, ErrNoSuchToken
	}
	backup := t.clone()
	applyInput(t, in)
	if strings.TrimSpace(t.Name) == "" {
		*t = backup
		return Token{}, errors.New("name is required")
	}
	if err := s.commit(); err != nil {
		*t = backup
		s.reindex()
		return Token{}, err
	}
	return t.clone(), nil
}

// Rotate replaces a token's secret, which immediately invalidates the old one.
func (s *Store) Rotate(id string) (Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.byID[id]
	if !ok {
		return Token{}, ErrNoSuchToken
	}
	backup := t.clone()
	t.Secret = NewSecret()
	if err := s.commit(); err != nil {
		*t = backup
		s.reindex()
		return Token{}, err
	}
	return t.clone(), nil
}

// Delete removes a token.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := slices.IndexFunc(s.tokens, func(t *Token) bool { return t.ID == id })
	if idx < 0 {
		return ErrNoSuchToken
	}
	backup := s.tokens
	s.tokens = slices.Delete(slices.Clone(s.tokens), idx, idx+1)
	if err := s.commit(); err != nil {
		s.tokens = backup
		s.reindex()
		return err
	}
	return nil
}

// TouchLastSeen records that an agent authenticated with this token. A failure
// to persist is not worth failing the connection over, so it is swallowed.
func (s *Store) TouchLastSeen(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	t.LastSeen = &now
	_ = s.persist()
}

// commit validates, reindexes and saves. Callers hold the write lock and must
// restore their own backup if it returns an error.
func (s *Store) commit() error {
	if err := s.validate(); err != nil {
		return err
	}
	s.reindex()
	return s.persist()
}

// applyInput copies the non-nil fields of in onto t.
func applyInput(t *Token, in TokenInput) {
	if in.Name != nil {
		t.Name = strings.TrimSpace(*in.Name)
	}
	if in.Subdomains != nil {
		subs := make([]string, 0, len(*in.Subdomains))
		for _, sub := range *in.Subdomains {
			sub = strings.ToLower(strings.TrimSpace(sub))
			if sub != "" && !slices.Contains(subs, sub) {
				subs = append(subs, sub)
			}
		}
		t.Subdomains = subs
	}
	if in.Ports != nil {
		ports := make([]int, 0, len(*in.Ports))
		for _, p := range *in.Ports {
			if !slices.Contains(ports, p) {
				ports = append(ports, p)
			}
		}
		t.Ports = ports
	}
	if in.MaxTunnels != nil {
		t.MaxTunnels = *in.MaxTunnels
	}
	if in.DenyTCP != nil {
		t.DenyTCP = *in.DenyTCP
	}
	if in.Disabled != nil {
		t.Disabled = *in.Disabled
	}
	if in.SSHKeys != nil {
		t.SSHKeys = slices.Clone(*in.SSHKeys)
	}
}

// ErrNoSuchToken is returned for an unknown token ID.
var ErrNoSuchToken = errors.New("no such token")

// MayUseSubdomain reports whether the token may claim sub, given the server's
// policy on unreserved names.
func (s *Store) MayUseSubdomain(tokenID, sub string, free bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.tokens {
		if !slices.Contains(t.Subdomains, sub) {
			continue
		}
		if t.ID != tokenID {
			return fmt.Errorf("subdomain %q is reserved", sub)
		}
		return nil
	}
	if !free {
		return fmt.Errorf("subdomain %q is not reserved for you and free subdomains are disabled", sub)
	}
	return nil
}

// MayUsePort mirrors MayUseSubdomain for fixed public TCP ports.
func (s *Store) MayUsePort(tokenID string, port int, free bool) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.tokens {
		if !slices.Contains(t.Ports, port) {
			continue
		}
		if t.ID != tokenID {
			return fmt.Errorf("port %d is reserved", port)
		}
		return nil
	}
	if !free {
		return fmt.Errorf("port %d is not reserved for you and free ports are disabled", port)
	}
	return nil
}

// ReservedPorts returns the ports reserved for a token, in order.
func (s *Store) ReservedPorts(tokenID string) []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[tokenID]
	if !ok {
		return nil
	}
	return slices.Clone(t.Ports)
}

// NewSecret returns a fresh agent secret.
func NewSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("server: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// validSubdomain enforces a single DNS label: lowercase alphanumerics and
// hyphens, not starting or ending with a hyphen.
func validSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// reservedSubdomains are never handed out to agents.
//
// The panel lives on the bare base domain, so no tunnel name can shadow it.
// What this list protects is the mail and DNS infrastructure of the zone: a
// tunnel answering on mail.<zone> would quietly break delivery. Ordinary names
// like "api" and "admin" are deliberately left available — they are exactly
// what people want to publish.
var reservedSubdomains = map[string]bool{
	"www": true, "mail": true, "smtp": true, "imap": true,
	"ns1": true, "ns2": true, "_acme-challenge": true,
}
