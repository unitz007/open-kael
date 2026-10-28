package domain

import "context"

type ctxAgentID struct{}

// WithAgentID records in ctx which agent a piece of work is for, so a hook the app supplies (such as the connect-link
// maker) can tailor what it does to that agent: a sign-in that covers every app of one provider the agent uses, say.
func WithAgentID(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, ctxAgentID{}, agentID)
}

// AgentIDFromContext is the agent WithAgentID recorded, or "".
func AgentIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxAgentID{}).(string)
	return id
}
