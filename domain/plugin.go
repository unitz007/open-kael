package domain

// PluginDeps bundles the shared, live dependencies a Plugin's Executor
// factory may need. Not every provider needs every field — an Executor
// factory with no bot/app identity concept (e.g. one backed only by
// per-user OAuth) simply ignores KindRegistry.
type PluginDeps struct {
	Resolver     CredentialResolver
	KindRegistry *IdentityKindRegistry
}

// Plugin bundles everything one provider integration contributes to the
// platform, in a shape every integration package can construct on its own:
// its Integration+Tools catalog entry (for seeding), its event catalogue
// (for skill triggers), an optional bot/app IdentityKind, and an Executor
// factory. A composition root collects one Plugin per provider and registers
// them into the relevant registries (IdentityKindRegistry, ExecutorRegistry,
// IntegrationEventRegistry, plus whatever seeds Integration/ToolDefinition
// records) in a single pass over the slice, instead of hand-maintaining a
// separate provider list per registry that drifts out of sync.
//
// Every field is optional except Service — a provider that only emits events
// and has no tools, no bot identity, and no Executor (e.g. a pure webhook
// source like Stripe) leaves the rest zero.
type Plugin struct {
	// Service is the provider key used throughout the platform — the same
	// string passed to ExecutorRegistry.Register, the provider half of an
	// IdentityKindRegistry key, IntegrationEventRegistry.Register, and
	// (conventionally) Integration.ID.
	Service string

	// Integration and Tools seed the platform's catalog. A nil Integration
	// means this provider contributes nothing an agent could bind a skill
	// to — only events.
	Integration *Integration
	Tools       []*ToolDefinition

	// Events is this provider's event catalogue, for providers that can act
	// as a webhook event source. Nil if it can't.
	Events []*IntegrationEvent

	// IdentityKind and IdentityKindType are set together, only for providers
	// with a bot/app-level Identity concept. IdentityKindType is one of the
	// IdentityKind* constants (IdentityKindBot, IdentityKindOAuth, ...).
	IdentityKind     IdentityKind
	IdentityKindType string

	// NewExecutor builds this provider's Executor from the shared deps. Nil
	// if this provider has no Executor (e.g. an event-source-only provider).
	NewExecutor func(deps PluginDeps) Executor
}
