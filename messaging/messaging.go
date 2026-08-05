package messaging

import (
	"context"

	"github.com/unitz007/kael/tools"
)

// ConversationRef addresses a specific chat/channel/DM on a specific
// platform — everything Send needs to route a reply to the right place.
// It is not used as a memory/threading key (see agent.Memory) — only
// ChatID is, deliberately excluding Platform from that concern.
type ConversationRef struct {
	Platform string // "telegram", "slack", ...
	ChatID   string // platform-specific chat/channel/DM id
}

// InboundMessage is what a Messenger hands back for each new message it
// receives while listening.
type InboundMessage struct {
	Conversation ConversationRef
	Text         string
	// MessageID is the platform's own id for this specific message (e.g.
	// Slack's "ts") — distinct from ConversationRef, which addresses the
	// conversation as a whole, not any one message within it. Empty when
	// the platform has nothing meaningful to react to per-message with (or
	// hasn't wired it up), which platform-specific tools that need it
	// (e.g. add_reaction) must handle explicitly.
	MessageID string
	// ThreadID is the id of the thread this message belongs (or, if it
	// starts one, would anchor) to — e.g. Slack's thread_ts. Distinct from
	// MessageID: a reply three levels into an existing thread has its own
	// MessageID but ThreadID pointing at the thread's root message, so a
	// reply can go back into the same thread rather than reacting to one
	// specific message in it. Empty on platforms with no threading concept.
	ThreadID string
}

// Messenger is a full bidirectional platform adapter: send to a specific
// conversation, and listen for new inbound ones. Telegram implements this
// today; Slack or anything else plugs in later against the same interface.
type Messenger interface {
	// Platform identifies this adapter for routing — matches ConversationRef.Platform.
	Platform() string
	// Send delivers text to a specific conversation on this platform.
	Send(ctx context.Context, conv ConversationRef, text string) error
	// Listen blocks, calling onMessage for each inbound message, until ctx
	// is cancelled, at which point it returns nil.
	Listen(ctx context.Context, onMessage func(InboundMessage)) error
	// DefaultConversation is where proactive/non-reply sends go — a
	// cron-triggered workflow, or anything else with no active conversation
	// to reply within.
	DefaultConversation() ConversationRef
}

// ToolProvider is implemented by a Messenger that offers extra tools beyond
// the built-in send_message — e.g. Slack's add_reaction/search_emoji, which
// have no cross-platform equivalent and so don't belong on the Messenger
// interface itself. Optional: a Messenger that doesn't implement this just
// contributes send_message, same as before this existed.
//
// Agent.baseTools() checks every registered messenger for this and merges
// in whatever it returns, so any agent with a matching messenger inherits
// its tools automatically — the same inheritance send_message already gets
// — rather than each agent's constructor needing to remember to AddTool
// them by hand.
type ToolProvider interface {
	Tools() []*tools.ToolSpec
}

type conversationCtxKey struct{}

// WithConversation attaches the active conversation to ctx, so a tool
// handler deep inside runLoopFrom (which only ever receives ctx, per
// tools.HandlerFunc) can find out where to reply.
func WithConversation(ctx context.Context, conv ConversationRef) context.Context {
	return context.WithValue(ctx, conversationCtxKey{}, conv)
}

// ConversationFromContext returns the conversation WithConversation
// attached, if any.
func ConversationFromContext(ctx context.Context) (ConversationRef, bool) {
	conv, ok := ctx.Value(conversationCtxKey{}).(ConversationRef)
	return conv, ok
}

type messageIDCtxKey struct{}

// WithMessageID attaches the id of the specific inbound message that
// triggered the current run to ctx — same reasoning as WithConversation,
// for tools that need to act on that one message (e.g. add_reaction)
// rather than the conversation as a whole.
func WithMessageID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, messageIDCtxKey{}, id)
}

// MessageIDFromContext returns the id WithMessageID attached, if any. ok is
// false both when nothing was attached and when an empty id was (a
// platform, or a proactive/cron-triggered run, with no specific message to
// point at).
func MessageIDFromContext(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(messageIDCtxKey{}).(string)
	return id, id != ""
}

type threadIDCtxKey struct{}

// WithThreadID attaches the current run's thread id to ctx — same
// reasoning as WithMessageID, so a Messenger's own Send implementation can
// reply into the same thread the triggering message came from, without
// Send's platform-agnostic signature needing a thread parameter every
// caller has to plumb through.
func WithThreadID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, threadIDCtxKey{}, id)
}

// ThreadIDFromContext returns the id WithThreadID attached, if any. ok is
// false both when nothing was attached and when an empty id was (a
// proactive/cron-triggered send, with no triggering message to thread
// under).
func ThreadIDFromContext(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(threadIDCtxKey{}).(string)
	return id, id != ""
}
