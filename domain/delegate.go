package domain

import "context"

// AgentDirectory resolves OTHER agents' Public Skills this Agent may call —
// the structured replacement for kael-platform's AgentDirectory/DelegateTarget
// (free-text DelegateCapabilities(), a whole agent handed a task string).
// Here the unit is one Skill, with the same typed InputSchema/OutputSchema
// contract as a local Skill call — no free-text task string anywhere in
// this path. Delegation is synchronous only: a returned BoundAction's
// Invoke blocks until the target agent's own Loop.Run (scoped to that one
// Skill) returns — the old framework's async/fire-and-forget and
// live-conversation-announce paths existed for Slack-thread UX that
// doesn't apply to a typed contract, so neither is carried over.
type AgentDirectory interface {
	// PublicSkills lists every OTHER agent's Public Skills this Agent may
	// delegate to, each already wrapped as a BoundAction whose Invoke
	// dispatches to the owning agent (locally or remotely — this interface
	// doesn't care which, same transport-agnostic spirit as
	// kael-platform's own DelegateTarget).
	PublicSkills(ctx context.Context) []*BoundAction
}

type delegationDepthKey struct{}

// MaxDelegationDepth bounds how many delegate calls may chain — a delegated
// Skill call's own action list should not include further delegation
// without limit. A plain call-depth counter threaded through ctx, mirroring
// the old framework's incrementDelegationDepth/maxDelegationDepth intent
// without new domain-level ceremony.
const MaxDelegationDepth = 5

// IncrementDelegationDepth returns a ctx carrying the incremented depth,
// the new depth, and whether it's still within MaxDelegationDepth. A
// concrete AgentDirectory implementation calls this before dispatching a
// delegate call, refusing (or omitting the action from PublicSkills
// entirely) once the limit is reached.
func IncrementDelegationDepth(ctx context.Context) (context.Context, int, bool) {
	depth, _ := ctx.Value(delegationDepthKey{}).(int)
	depth++
	return context.WithValue(ctx, delegationDepthKey{}, depth), depth, depth <= MaxDelegationDepth
}
