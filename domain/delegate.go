package domain

import "context"

// AgentDirectory resolves OTHER agents' Public Skills this Agent may call.
// Here the unit is one Skill, with the same typed InputSchema/OutputSchema
// contract as a local Skill call — no free-text task string anywhere in
// this path. Delegation is synchronous only: a returned BoundAction's
// Invoke blocks until the target agent's own Loop.Run (scoped to that one
// Skill) returns.
//
// Not wired up in the Sports Agent pilot — Sports agent's delegation, if
// any, continues to go through kael-platform's own AgentDirectory/
// message_agent, untouched.
type AgentDirectory interface {
	// PublicSkills lists every OTHER agent's Public Skills this Agent may
	// delegate to, each already wrapped as a BoundAction whose Invoke
	// dispatches to the owning agent (locally or remotely — this interface
	// doesn't care which).
	PublicSkills(ctx context.Context) []*BoundAction
}

type delegationDepthKey struct{}

// MaxDelegationDepth bounds how many delegate calls may chain — a delegated
// Skill call's own action list should not include further delegation
// without limit.
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
