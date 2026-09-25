package domain

import "context"

// CredentialResolver resolves an opaque credential reference
// (stored on Identity.CredentialRef or AppAuthorization.CredentialRef) into
// the real secret value. The reference is an opaque pointer into whatever
// credential store the platform uses — callers never see raw secrets.
type CredentialResolver interface {
	ResolveToken(ctx context.Context, ref string) (string, error)
}

// CredentialEncryptor encrypts a raw secret into the opaque reference
// format that Identity.CredentialRef and AppAuthorization.CredentialRef store.
// Kept separate from CredentialResolver so the API layer (which needs to
// store secrets) and the executor layer (which needs to read them) can each
// depend on only the half they use.
type CredentialEncryptor interface {
	EncryptToken(ctx context.Context, plaintext string) (string, error)
}

// IdentityKind encapsulates the authentication flow for one kind of Identity.
// Concrete implementations live in executor or kind packages — not in domain —
// so they can import provider-specific crypto and HTTP logic without polluting
// the domain layer.
type IdentityKind interface {
	// Authenticate returns the bearer token (or equivalent credential) to
	// use for a single API call. identity carries the app-level credential
	// (bot token PEM, OAuth client creds, etc.); connectionRef is the
	// per-user credential reference from an AppAuthorization (installation
	// ID, user access token ref) — empty for bot-only auth.
	Authenticate(ctx context.Context, resolver CredentialResolver, identity *Identity, connectionRef string) (string, error)
}

// IdentityKindRegistry maps (provider, kind) pairs to their live IdentityKind
// implementations. Keyed on "provider:kind" so different providers can
// register separate implementations for the same kind name (e.g. GitHub's
// "bot" does JWT exchange; Telegram's "bot" just resolves a credential ref).
// Executors consult it instead of branching on provider/kind strings.
type IdentityKindRegistry struct {
	kinds map[string]IdentityKind
}

func NewIdentityKindRegistry() *IdentityKindRegistry {
	return &IdentityKindRegistry{kinds: make(map[string]IdentityKind)}
}

// Register stores impl under the (provider, kind) pair.
func (r *IdentityKindRegistry) Register(provider, kind string, impl IdentityKind) {
	r.kinds[provider+":"+kind] = impl
}

// For returns the IdentityKind registered for (provider, kind), or nil, false
// when none is registered.
func (r *IdentityKindRegistry) For(provider, kind string) (IdentityKind, bool) {
	k, ok := r.kinds[provider+":"+kind]
	return k, ok
}
