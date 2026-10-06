package domain

import "context"

// Executor performs one service's tools — implemented once per service
// (e.g. "slack", "github", "discord"), not once per tool or identity.
//
// identity is the creator-owned app credential (the GitHub App, Slack App,
// Telegram Bot registered with the provider). connectionRef is the opaque
// credential reference resolved from the user's AppAuthorization (OAuth token,
// installation ID, workspace bot token) or empty for bot-only messaging where
// the Identity credential is sufficient. Either may be empty when not
// applicable for a given service.
type Executor interface {
	Execute(ctx context.Context, identity *Identity, connectionRef string, action string, input map[string]any) (any, error)
}

// MessengerSkillProvider is implemented by Executor types that provide
// messenger-native private skills. The returned name (e.g. "Slack",
// "Telegram") labels the skill in the system prompt; the actions are
// appended to the turn's tool list. The skill is private — the LLM can
// call its tools but must not describe or list them to users.
// The runtime calls it once per turn with the resolved Identity and
// ConversationRef.
type MessengerSkillProvider interface {
	MessengerSkill(ctx context.Context, identity *Identity, conv ConversationRef) (name string, actions []*BoundAction)
}

// ExecutorRegistry looks up the Executor responsible for one service.
// One registry entry per service, shared across every Identity of that
// service — e.g. multiple GitHub App identities all resolve to the same
// GitHubExecutor, differentiated at call time by which identity and
// connectionRef are passed in.
type ExecutorRegistry struct {
	executors map[string]Executor
}

func NewExecutorRegistry() *ExecutorRegistry {
	return &ExecutorRegistry{executors: make(map[string]Executor)}
}

func (r *ExecutorRegistry) Register(service string, e Executor) {
	r.executors[service] = e
}

// For returns the Executor registered for service (e.g. "github", "slack").
func (r *ExecutorRegistry) For(service string) (Executor, bool) {
	e, ok := r.executors[service]
	return e, ok
}
