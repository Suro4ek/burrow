package server

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func TestStoreCreateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	tok, err := s.Create(TokenInput{
		Name:       ptr("laptop"),
		Subdomains: ptr([]string{"Dev", " api "}),
		Ports:      ptr([]int{25343}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Secret) < 16 {
		t.Errorf("generated secret is too short: %q", tok.Secret)
	}
	// Reservations are normalised on the way in, so lookups never have to
	// worry about case or stray whitespace.
	if !slices.Equal(tok.Subdomains, []string{"dev", "api"}) {
		t.Errorf("subdomains = %v, want [dev api]", tok.Subdomains)
	}

	// A second process must see the same data.
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Lookup(tok.Secret)
	if !ok {
		t.Fatal("token did not survive a reopen")
	}
	if got.Name != "laptop" || got.ID != tok.ID {
		t.Errorf("reopened token = %+v, want laptop/%s", got, tok.ID)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("tokens file mode = %o, want 600", perm)
	}
}

func TestStoreRejectsDuplicateReservation(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(TokenInput{Name: ptr("a"), Subdomains: ptr([]string{"dev"})}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create(TokenInput{Name: ptr("b"), Subdomains: ptr([]string{"dev"})})
	if err == nil {
		t.Fatal("expected the second reservation of \"dev\" to fail")
	}
	// The rejected token must not have been half-added.
	if n := s.Count(); n != 1 {
		t.Errorf("store has %d tokens, want 1", n)
	}
}

func TestStoreUpdateRollsBackOnConflict(t *testing.T) {
	s := newStore(t)
	a, err := s.Create(TokenInput{Name: ptr("a"), Subdomains: ptr([]string{"dev"})})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(TokenInput{Name: ptr("b")})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Update(b.ID, TokenInput{Subdomains: ptr([]string{"dev"})}); err == nil {
		t.Fatal("expected taking another token's reservation to fail")
	}

	// b must be untouched, and a must still own "dev".
	after, _ := s.Get(b.ID)
	if len(after.Subdomains) != 0 {
		t.Errorf("failed update leaked state: %v", after.Subdomains)
	}
	if err := s.MayUseSubdomain(a.ID, "dev", false); err != nil {
		t.Errorf("original owner lost its reservation: %v", err)
	}
}

func TestStorePermissions(t *testing.T) {
	s := newStore(t)
	owner, _ := s.Create(TokenInput{
		Name:       ptr("owner"),
		Subdomains: ptr([]string{"dev"}),
		Ports:      ptr([]int{25343}),
	})
	other, _ := s.Create(TokenInput{Name: ptr("other")})

	if err := s.MayUseSubdomain(owner.ID, "dev", false); err != nil {
		t.Errorf("owner was refused its own subdomain: %v", err)
	}
	if err := s.MayUseSubdomain(other.ID, "dev", true); err == nil {
		t.Error("a reserved subdomain was handed to another token")
	}
	// An unreserved name depends purely on the free-subdomains policy.
	if err := s.MayUseSubdomain(other.ID, "scratch", true); err != nil {
		t.Errorf("free subdomain refused: %v", err)
	}
	if err := s.MayUseSubdomain(other.ID, "scratch", false); err == nil {
		t.Error("free subdomains were disabled but the claim succeeded")
	}

	if err := s.MayUsePort(owner.ID, 25343, false); err != nil {
		t.Errorf("owner was refused its own port: %v", err)
	}
	if err := s.MayUsePort(other.ID, 25343, true); err == nil {
		t.Error("a reserved port was handed to another token")
	}
}

func TestStoreRotateInvalidatesOldSecret(t *testing.T) {
	s := newStore(t)
	tok, _ := s.Create(TokenInput{Name: ptr("laptop")})
	old := tok.Secret

	rotated, err := s.Rotate(tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == old {
		t.Fatal("rotate returned the same secret")
	}
	if _, ok := s.Lookup(old); ok {
		t.Error("the old secret still authenticates")
	}
	if _, ok := s.Lookup(rotated.Secret); !ok {
		t.Error("the new secret does not authenticate")
	}
}

func TestStoreDelete(t *testing.T) {
	s := newStore(t)
	tok, _ := s.Create(TokenInput{Name: ptr("temp")})

	if err := s.Delete(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(tok.Secret); ok {
		t.Error("deleted token still authenticates")
	}
	if err := s.Delete(tok.ID); err == nil {
		t.Error("deleting a missing token should fail")
	}
}

// TestStoreMigratesLegacyFile covers files written before tokens had IDs.
func TestStoreMigratesLegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	legacy := `[{"token":"legacy-secret-0123456789","name":"old","subdomains":["Dev"]}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := s.Lookup("legacy-secret-0123456789")
	if !ok {
		t.Fatal("legacy token did not load")
	}
	if tok.ID == "" {
		t.Error("migration did not assign an id")
	}
	if tok.CreatedAt.IsZero() {
		t.Error("migration did not assign a creation time")
	}
	if !slices.Equal(tok.Subdomains, []string{"dev"}) {
		t.Errorf("subdomains = %v, want [dev]", tok.Subdomains)
	}
}

func TestStoreRejectsServerReservedSubdomain(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(TokenInput{Name: ptr("x"), Subdomains: ptr([]string{"www"})}); err == nil {
		t.Fatal("expected \"www\" to be refused")
	}
}
