package identity

import "context"

// Identity is a set of credentials an agent acts as on some external
// system — a specific GitHub App installation, a specific AWS role, etc.
// Deliberately bare: unlike messaging.Messenger, there's no common action
// shape across arbitrary providers (opening a GitHub PR and deploying to
// AWS share nothing), so Identity doesn't define any actions itself — its
// only job is declaring who an agent is and handing back a currently-valid
// credential. Token is a method, not a field, so an implementation can
// transparently mint/refresh short-lived credentials (a GitHub App
// installation token lasts about an hour) instead of a caller ever reading
// a value that's silently gone stale.
type Identity interface {
	Provider() string                        // "github", "aws", ...
	ActingAs() string                         // the account/username/role this identity presents as
	Token(ctx context.Context) (string, error) // a currently-valid credential, refreshed internally if needed
}

// ctxKey threads an agent's registered identities through ctx. This exists
// for the same reason messaging.WithConversation does: a tool can be built
// completely independently of any specific *Agent — a workflow's own tools
// are constructed before the owning agent even exists — so there's no
// *Agent reference to close over at construction time. Threading identities
// through ctx instead means they're resolved only once a specific agent's
// loop is actually running, by whichever tool needs them, regardless of
// where that tool was originally built.
type ctxKey struct{}

// WithIdentities attaches a set of identities (keyed by Provider()) to ctx.
func WithIdentities(ctx context.Context, ids map[string]Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, ids)
}

// FromContext looks up a registered identity by provider from whichever
// agent's loop is currently executing. ok is false if ctx carries no
// identities at all, or none registered for that provider.
func FromContext(ctx context.Context, provider string) (Identity, bool) {
	ids, _ := ctx.Value(ctxKey{}).(map[string]Identity)
	id, ok := ids[provider]
	return id, ok
}
