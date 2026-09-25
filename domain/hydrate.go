package domain

import (
	"context"
	"fmt"
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
		}
		return executor.Execute(ctx, identity, connectionRef, def.Action, input)
	}
	if def.RequiresApproval {
		invoke = withApprovalGate(def, invoke)
	}

	return &BoundAction{
		Spec: ActionSpec{
			Name:         def.Name,
			Description:  def.Description,
			InputSchema:  def.InputSchema,
			OutputSchema: def.OutputSchema,
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

// resolveIdentityForIntegration picks the first Identity from agent.IdentityIDs
// whose IntegrationID matches integration.ID. Returns nil when the agent has
// no identity configured for this integration (tool runs without app-level auth).
func resolveIdentityForIntegration(agent *Agent, integration *Integration, identitiesByID map[string]*Identity) *Identity {
	if agent == nil {
		return nil
	}
	for _, identityID := range agent.IdentityIDs {
		identity, ok := identitiesByID[identityID]
		if ok && identity.IntegrationID == integration.ID {
			return identity
		}
	}
	return nil
}

type ctxConnectionRefs struct{}
type ctxUserID struct{}

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
