package domain

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
)

// HydrateTool resolves a stored ToolDefinition into a real, callable
// BoundAction via the Executor registered for integration.Service.
// identity is the agent-selected credential for this integration; it may be
// nil for tools that need no app-level identity.
//
// connectionRef is resolved at invocation time from context (the runtime sets
// the current user's AppAuthorization credential per turn via
// WithConnectionRef) rather than at hydration time — this is what makes
// multi-tenancy work: the same BoundAction serves every user, each getting
// their own credential resolved on the fly.
func HydrateTool(def *ToolDefinition, identity *Identity, integration *Integration, registry *ExecutorRegistry) (*BoundAction, error) {
	if def.IntegrationID != integration.ID {
		return nil, fmt.Errorf("hydrate tool %q: belongs to integration %q, got %q", def.ID, def.IntegrationID, integration.ID)
	}
	executor, ok := registry.For(integration.Service)
	if !ok {
		return nil, fmt.Errorf("hydrate tool %q: no executor registered for service %q", def.ID, integration.Service)
	}

	invoke := func(ctx context.Context, input map[string]any) (any, error) {
		// Resolve the user's connection ref at call time — set by the runtime
		// host per turn from the user's AppAuthorization for this identity.
		var connectionRef string
		if identity != nil {
			connectionRef, _ = ConnectionRefFromContext(ctx, identity.ID)
			// Stamp the identity ID so executor callbacks (e.g. OnTokenRotated)
			// can fall back to an identity-scoped DB lookup when the stored
			// credential ref has already been rotated by an earlier tool call.
			ctx = WithCallerIdentity(ctx, identity.ID)
		}
		return executor.Execute(ctx, identity, connectionRef, def.Action, input)
	}
	// Auth gate: if identity requires a user connection ref and none is set,
	// prompt the user to connect their account before executing the tool.
	if identity != nil {
		invoke = withAuthGate(identity, def.Name, invoke)
	}
	// User-level approval gate runs first (checked at call time from context)
	// so a user can add approval to any tool regardless of its definition.
	invoke = withUserApprovalGate(def, invoke)
	if def.RequiresApproval {
		invoke = withApprovalGate(def, invoke)
		// Outermost wrapper: short-circuit re-calls after first success so
		// the model cannot loop on an approved write action.
		invoke = withOneShotGuard(invoke)
	}

	name := def.FunctionName
	if name == "" {
		name = def.Name
	}
	return &BoundAction{
		Spec: ActionSpec{
			Name:         name,
			Description:  def.Description,
			Instructions: def.Instructions,
			InputSchema:  def.InputSchema,
			OutputSchema: def.OutputSchema,
			MaxCalls:     def.MaxCalls,
		},
		Invoke: invoke,
	}, nil
}

// withApprovalGate wraps invoke with a human-approval check. A tool marked
// RequiresApproval must be approved before its action runs — refusing to
// execute silently when no ApprovalRequester is attached.
func withApprovalGate(def *ToolDefinition, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		requester, ok := ApprovalRequesterFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("tool %q requires approval, but this run has no ApprovalRequester attached — refusing to run rather than executing unapproved", def.ID)
		}
		conv, ok := ConversationFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("tool %q requires approval, but this run has no active conversation to ask in", def.ID)
		}
		approved, err := requester.RequestApproval(ctx, conv, def.RenderApprovalPrompt(input), def.ApprovalTimeoutSeconds)
		if err != nil {
			return nil, fmt.Errorf("tool %q: requesting approval: %w", def.ID, err)
		}
		if !approved {
			return "not approved", nil
		}
		return inner(ctx, input)
	}
}

// withOneShotGuard wraps a RequiresApproval action so it can only execute
// once per BoundAction lifetime (one turn). After the first successful call,
// subsequent calls return a directive telling the model to call finish rather
// than re-running the action — preventing approval-gated write tools from
// looping when the model generates slightly different arguments each time.
func withOneShotGuard(inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	var done atomic.Bool
	return func(ctx context.Context, input map[string]any) (any, error) {
		if done.Load() {
			log.Printf("one-shot guard: blocking re-call (already succeeded)")
			return "already completed — call finish to deliver the result to the user", nil
		}
		output, err := inner(ctx, input)
		if err == nil {
			log.Printf("one-shot guard: success, marking done")
			done.Store(true)
		} else {
			log.Printf("one-shot guard: inner returned error, done stays false: %v", err)
		}
		return output, err
	}
}

// WrapOneShotGuard wraps action so it can only execute once per BoundAction
// lifetime. After the first successful call, subsequent calls return a
// directive telling the model to call finish. Used by the host to prevent
// the outer NativeLoop from calling the same skill more than once per turn
// when falling back from the skill router.
func WrapOneShotGuard(action *BoundAction) *BoundAction {
	return &BoundAction{
		Spec:   action.Spec,
		Invoke: withOneShotGuard(action.Invoke),
	}
}

// HydrateSkillTools resolves every Tool a Skill binds into BoundActions.
// agent supplies the IdentityIDs used to pick the right Identity per
// integration. toolsByID, identitiesByID, and integrationsByID are plain
// lookup maps — this function doesn't care where they came from.
func HydrateSkillTools(skill *Skill, agent *Agent, toolsByID map[string]*ToolDefinition, identitiesByID map[string]*Identity, integrationsByID map[string]*Integration, registry *ExecutorRegistry) ([]*BoundAction, error) {
	bound := make([]*BoundAction, 0, len(skill.Tools))
	for _, binding := range skill.Tools {
		def, ok := toolsByID[binding.ToolID]
		if !ok {
			return nil, fmt.Errorf("skill %q: unknown tool %q", skill.ID, binding.ToolID)
		}
		integration, ok := integrationsByID[def.IntegrationID]
		if !ok {
			return nil, fmt.Errorf("skill %q: tool %q references unknown integration %q", skill.ID, def.ID, def.IntegrationID)
		}
		identity := resolveIdentityForIntegration(agent, integration, identitiesByID)
		action, err := HydrateTool(def, identity, integration, registry)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", skill.ID, err)
		}
		bound = append(bound, action)
	}
	return bound, nil
}

// ConnectToolNameFromContext retrieves the name of the tool that triggered the
// auth gate, set by withAuthGate so the ConnectRequester can craft a contextual
// connect prompt. Returns empty string when not set.
func ConnectToolNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(ctxConnectToolName{}).(string)
	return name
}

// withAuthGate wraps invoke with a just-in-time authentication check. When the
// user's connectionRef for identity is empty, it asks the user to connect their
// account (via ConnectRequester from context) and blocks until connected or the
// request times out. On success it injects the fresh connectionRef into context
// so the inner invoke picks it up via ConnectionRefFromContext and calls the
// executor with the real credential. Falls through silently when no
// ConnectRequester is attached (e.g. cron/event runs with no user in context).
// toolName is stored in context so the ConnectRequester can reference the
// triggering tool in its connect prompt.
func withAuthGate(identity *Identity, toolName string, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		connectionRef, _ := ConnectionRefFromContext(ctx, identity.ID)
		if connectionRef != "" {
			return inner(ctx, input)
		}
		requester, ok := ConnectRequesterFromContext(ctx)
		if !ok {
			return inner(ctx, input)
		}
		conv, ok := ConversationFromContext(ctx)
		if !ok {
			return inner(ctx, input)
		}
		userID := UserIDFromContext(ctx)
		if toolName != "" {
			ctx = context.WithValue(ctx, ctxConnectToolName{}, toolName)
		}
		newRef, err := requester.RequestConnect(ctx, conv, identity, userID)
		if err != nil {
			return nil, fmt.Errorf("tool requires authentication — %w", err)
		}
		if newRef == "" {
			return "authentication required — please connect your account and try again", nil
		}
		// Inject the new ref so the inner executor call picks it up.
		refs, _ := ctx.Value(ctxConnectionRefs{}).(map[string]string)
		freshRefs := make(map[string]string, len(refs)+1)
		for k, v := range refs {
			freshRefs[k] = v
		}
		freshRefs[identity.ID] = newRef
		return inner(WithConnectionRefs(ctx, freshRefs), input)
	}
}

// resolveIdentityForIntegration picks the first Identity from agent.IdentityIDs
// whose IntegrationID matches integration.ID. When no agent-level identity is
// found (user-auth-only integrations never appear in agent.IdentityIDs), it
// falls back to searching identitiesByID so the auth gate can still resolve a
// connectionRef and generate a connect URL for those integrations.
func resolveIdentityForIntegration(agent *Agent, integration *Integration, identitiesByID map[string]*Identity) *Identity {
	if agent != nil {
		for _, identityID := range agent.IdentityIDs {
			identity, ok := identitiesByID[identityID]
			if ok && identity.IntegrationID == integration.ID {
				return identity
			}
		}
	}
	// Fallback: find any identity for this integration — covers user-auth-only
	// integrations whose identities are never added to agent.IdentityIDs.
	for _, identity := range identitiesByID {
		if identity.IntegrationID == integration.ID {
			return identity
		}
	}
	return nil
}

type ctxConnectionRefs struct{}
type ctxUserID struct{}
type ctxCallerIdentity struct{}
type ctxUserToolApprovals struct{}
type ctxConnectToolName struct{}

// WithConnectionRefs stores a map of identityID → connectionRef in ctx so
// BoundAction invocations can resolve the current user's credential at call
// time. The runtime host sets this per turn from the user's AppAuthorizations.
func WithConnectionRefs(ctx context.Context, refs map[string]string) context.Context {
	return context.WithValue(ctx, ctxConnectionRefs{}, refs)
}

// ConnectionRefFromContext retrieves the connectionRef for a specific identity
// from the per-turn context set by WithConnectionRefs. Returns empty string
// when no authorization exists for that identity (bot-only tools are fine
// with an empty connectionRef).
func ConnectionRefFromContext(ctx context.Context, identityID string) (string, bool) {
	refs, ok := ctx.Value(ctxConnectionRefs{}).(map[string]string)
	if !ok {
		return "", false
	}
	ref, ok := refs[identityID]
	return ref, ok
}

// WithUserID stores the platform user ID in ctx so executors that need to
// persist per-user state (e.g. rotated OAuth tokens) can retrieve it at call
// time without changing the Execute signature.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserID{}, userID)
}

// UserIDFromContext retrieves the user ID set by WithUserID. Returns empty
// string when not set (bot-triggered or test contexts without a real user).
func UserIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxUserID{}).(string)
	return id
}

// WithCallerIdentity stores the identity ID of the Identity whose credential
// is being resolved for the current tool invocation. HydrateTool sets this
// inside the BoundAction's invoke closure so executors and their callbacks
// can perform identity-scoped fallback lookups (e.g. re-finding an auth
// record after a credential rotation changed the stored ref).
func WithCallerIdentity(ctx context.Context, identityID string) context.Context {
	return context.WithValue(ctx, ctxCallerIdentity{}, identityID)
}

// CallerIdentityFromContext retrieves the identity ID set by WithCallerIdentity.
// Returns empty string when not set (tools invoked without an app identity).
func CallerIdentityFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxCallerIdentity{}).(string)
	return id
}

// WithUserToolApprovals stores a set of tool IDs the current user has
// configured to require approval. The runtime host sets this per turn
// alongside WithConnectionRefs. withUserApprovalGate reads it at call time,
// so any tool in this set gets the approval gate even if its ToolDefinition
// has RequiresApproval: false.
func WithUserToolApprovals(ctx context.Context, toolIDs map[string]bool) context.Context {
	return context.WithValue(ctx, ctxUserToolApprovals{}, toolIDs)
}

// UserToolApprovalsFromContext retrieves the user's per-tool approval set.
func UserToolApprovalsFromContext(ctx context.Context) map[string]bool {
	m, _ := ctx.Value(ctxUserToolApprovals{}).(map[string]bool)
	return m
}

// withUserApprovalGate wraps invoke with a gate that fires when the current
// user has flagged def.ID in their per-tool approval preferences (stored in
// context via WithUserToolApprovals). It is a no-op for turns where no user
// approval set is present (cron, event, bot-only) or where the tool is not in
// the set — so it is safe to attach to every tool unconditionally.
func withUserApprovalGate(def *ToolDefinition, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		approvals := UserToolApprovalsFromContext(ctx)
		if !approvals[def.ID] {
			return inner(ctx, input)
		}
		requester, ok := ApprovalRequesterFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("tool %q: user requires approval, but this run has no ApprovalRequester attached", def.ID)
		}
		conv, ok := ConversationFromContext(ctx)
		if !ok {
			return nil, fmt.Errorf("tool %q: user requires approval, but this run has no active conversation", def.ID)
		}
		approved, err := requester.RequestApproval(ctx, conv, def.RenderApprovalPrompt(input), def.ApprovalTimeoutSeconds)
		if err != nil {
			return nil, fmt.Errorf("tool %q: requesting user approval: %w", def.ID, err)
		}
		if !approved {
			return "not approved", nil
		}
		return inner(ctx, input)
	}
}
