package domain

import (
	"context"
	"time"
)

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
	// Shared: others read this chat (ConversationRef.Shared), so its history is only what was said in it.
	Shared bool
}

// Memory holds an Agent's conversation turns across separate inbound
// messages. No implementation ships here — a concrete store (Postgres,
// in-process, file-backed) belongs to whoever embeds this framework.
type Memory interface {
	History(ctx context.Context, key ConversationKey) []Message
	Append(ctx context.Context, key ConversationKey, messages ...Message)
}

// PersonChats is kept by a Memory that can: the chats a person has written to a minion in, the one they wrote in last
// first (at most 20), so what the minion sends them unasked (an alert, a repeating task) reaches them where they are
// now rather than where they set it up (Host.ReachAt).
type PersonChats interface {
	ChatsOf(ctx context.Context, agentID, userID string) []PersonChat
}

// PersonChat is one chat a person has written to a minion in.
type PersonChat struct {
	IdentityID string
	ChatID     string
}

// FollowUps is kept by a Memory that can: which of an agent's capabilities a conversation's latest message was routed
// to, and when, so a short follow-up ("4h", "and tomorrow?") stays with them, also when the reply before used none of
// their abilities (it asked something back), on every server and after a restart. A Memory without it falls back to
// what the conversation's messages used.
type FollowUps interface {
	LastSkills(ctx context.Context, key ConversationKey) (skills []string, at time.Time)
	SetLastSkills(ctx context.Context, key ConversationKey, skills []string)
}
