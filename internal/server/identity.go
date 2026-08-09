package server

import (
	"context"
	"fmt"
	"slices"
)

// Identity is who an agent is and what it is allowed to publish.
//
// It is deliberately the same shape whether it came from the local token file
// or from an external control plane, so nothing downstream has to care which.
type Identity struct {
	// ID is stable for this credential and is what usage is attributed to.
	ID string
	// Name is for logs and the panel.
	Name string
	// Subdomains and Ports are reservations: names this identity may claim.
	Subdomains []string
	Ports      []int
	// MaxTunnels caps concurrent tunnels; 0 means only the server-wide limit.
	MaxTunnels int
	// DenyTCP restricts the identity to HTTP tunnels.
	DenyTCP bool
}

// AuthRequest is what an agent presented when it connected.
type AuthRequest struct {
	Token        string
	AgentVersion string
	Hostname     string
	RemoteAddr   string
}

// ErrDenied rejects an agent. Its message is shown to the agent verbatim, so
// it should read as an explanation rather than an internal error.
type ErrDenied struct{ Reason string }

func (e *ErrDenied) Error() string { return e.Reason }

// denied is shorthand for building an ErrDenied.
func denied(reason string) error { return &ErrDenied{Reason: reason} }

// Authenticator resolves an agent's secret into an identity.
//
// Implementations are the local token store and an HTTP hook into an external
// control plane; the latter is what lets a hosted service own accounts and
// plans without any of that logic living in the tunnel path.
type Authenticator interface {
	// Authenticate is called once per agent connection.
	Authenticate(ctx context.Context, req AuthRequest) (Identity, error)
	// Refresh re-reads an already-authenticated identity, so that changing a
	// limit does not require the agent to reconnect. Implementations that
	// cannot cheaply re-read may return the identity they last issued.
	Refresh(ctx context.Context, id string) (Identity, error)
	// AllowSubdomain and AllowPort decide whether an identity may claim a
	// specific name. They belong here rather than on Identity because the
	// answer depends on what *other* identities have reserved, which only the
	// backend knows.
	AllowSubdomain(ctx context.Context, id Identity, sub string, free bool) error
	AllowPort(ctx context.Context, id Identity, port int, free bool) error
}

// storeAuth is the default Authenticator, backed by the local token file.
type storeAuth struct{ store *Store }

func (a storeAuth) Authenticate(_ context.Context, req AuthRequest) (Identity, error) {
	tok, ok := a.store.Lookup(req.Token)
	if !ok {
		return Identity{}, denied("invalid token")
	}
	if tok.Disabled {
		return Identity{}, denied("this token is disabled")
	}
	a.store.TouchLastSeen(tok.ID)
	return identityOf(tok), nil
}

func (a storeAuth) Refresh(_ context.Context, id string) (Identity, error) {
	tok, ok := a.store.Get(id)
	if !ok {
		return Identity{}, denied("token no longer exists")
	}
	if tok.Disabled {
		return Identity{}, denied("this token is disabled")
	}
	return identityOf(tok), nil
}

func identityOf(t Token) Identity {
	return Identity{
		ID:         t.ID,
		Name:       t.Name,
		Subdomains: slices.Clone(t.Subdomains),
		Ports:      slices.Clone(t.Ports),
		MaxTunnels: t.MaxTunnels,
		DenyTCP:    t.DenyTCP,
	}
}

// AllowSubdomain defers to the token file, which can also see reservations
// held by other tokens.
func (a storeAuth) AllowSubdomain(_ context.Context, id Identity, sub string, free bool) error {
	return a.store.MayUseSubdomain(id.ID, sub, free)
}

// AllowPort mirrors AllowSubdomain for fixed public TCP ports.
func (a storeAuth) AllowPort(_ context.Context, id Identity, port int, free bool) error {
	return a.store.MayUsePort(id.ID, port, free)
}

// ownReservations answers from the identity alone. An external control plane
// is authoritative over who reserved what, so all this can check is whether it
// granted the name; conflicts between live tunnels are caught by the registry.
func ownReservations(reserved bool, what string, free bool) error {
	if reserved || free {
		return nil
	}
	return fmt.Errorf("%s is not reserved for you and free allocation is disabled", what)
}
