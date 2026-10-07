package domain

import "context"

// ConversationKey uniquely identifies one conversation. UserID (when set)
// broadens the scope to all channels for that user+agent pair, giving a
// unified history regardless of which platform the message arrived on.
// ThreadID (when set) narrows back to a single thread — used for Slack
// thread replies where only the thread's own context is relevant.
type ConversationKey struct {
	AgentID    string
	IdentityID string
	ChatID     string
	ThreadID   string
	UserID     string
}

// Memory holds an Agent's conversation turns across separate inbound
// messages. No implementation ships here — a concrete store (Postgres,
// in-process, file-backed) belongs to whoever embeds this framework.
type Memory interface {
	History(ctx context.Context, key ConversationKey) []Message
	Append(ctx context.Context, key ConversationKey, messages ...Message)
}
