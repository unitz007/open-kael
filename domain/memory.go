package domain

import "context"

// ConversationKey uniquely identifies one conversation: the agent handling it,
// the specific bot identity it arrived on, the chat (channel or DM), and the
// thread within that chat (empty for unthreaded providers).
type ConversationKey struct {
	AgentID    string
	IdentityID string
	ChatID     string
	ThreadID   string
}

// Memory holds an Agent's conversation turns across separate inbound
// messages. No implementation ships here — a concrete store (Postgres,
// in-process, file-backed) belongs to whoever embeds this framework.
type Memory interface {
	History(ctx context.Context, key ConversationKey) []Message
	Append(ctx context.Context, key ConversationKey, messages ...Message)
}
