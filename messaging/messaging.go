package messaging

import (
	"context"

	"github.com/unitz007/kael/tools"
)

// ConversationRef addresses a specific chat/channel/DM on a specific
// platform — everything Send needs to route a reply to the right place.
// Also feeds into memory partitioning (see MemoryKeyFunc) alongside
// whatever else a given strategy reads off ctx.
type ConversationRef struct {
	Platform      string // "telegram", "slack", ...
	ChatID        string // platform-specific chat/channel/DM id
	DirectMessage bool   // true when this is a 1:1 DM, not a group/channel
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
	// WorkflowID is set only when this message is a genuine reply within a
	// thread whose root message was tagged as having come from a specific
	// workflow (a Messenger-specific mechanism — e.g. Slack message
	// metadata; see KeyByWorkflow). Empty for ordinary chat, and empty on
	// platforms with no equivalent tagging mechanism.
	WorkflowID string
}

// MessengerResponse is what Send/Reply return once a message is actually
// posted. A struct rather than a bare string so a future field (e.g. a
// permalink, once some platform has an equivalent) doesn't need another
// Messenger interface signature change — every existing caller destructures
// by field name, so an addition here is source-compatible.
type MessengerResponse struct {
	// MessageID is the platform's own id for the message that was just
	// posted (e.g. Slack's ts, Telegram's message_id) — the same kind of
	// value InboundMessage.MessageID carries for an inbound one. Lets a
	// caller that needs to find this exact message again later (e.g.
	// job_application_wf recording which Slack message a job notification
	// landed as, so a future reply can be matched back to it) do so
	// without a platform-specific escape hatch outside this interface.
	MessageID string
}

// Messenger is a full bidirectional platform adapter: send to a specific
// conversation, and listen for new inbound ones. This package ships no
// implementations — see examples/messenger for a working Slack/Telegram
// reference to copy, same reasoning identity.Identity and webhook.Source
// ship no implementations either.
type Messenger interface {
	// Platform identifies this adapter for routing — matches ConversationRef.Platform.
	Platform() string
	// Send delivers text as a fresh, un-threaded message to a specific
	// conversation on this platform — a cron/webhook-triggered workflow's
	// own notification, or anything else with no specific inbound message
	// to address the reply back to. Use Reply instead whenever there is
	// one.
	Send(ctx context.Context, conv ConversationRef, text string) (MessengerResponse, error)
	// Reply delivers text addressed back to a specific inbound message —
	// every platform has some mechanism for this (Slack's thread_ts,
	// Telegram's reply_parameters, WhatsApp's context.message_id, email's
	// In-Reply-To), even though what they each key off differs (Slack
	// threads off a shared root; most others point at the specific
	// message being answered). A Messenger with nothing meaningful to
	// address back to (to.MessageID/to.ThreadID both empty) should behave
	// like a plain Send.
	Reply(ctx context.Context, to InboundMessage, text string) (MessengerResponse, error)
	// Listen blocks, calling onMessage for each inbound message, until ctx
	// is cancelled, at which point it returns nil.
	Listen(ctx context.Context, onMessage func(InboundMessage)) error
	// DefaultConversation is where proactive/non-reply sends go — a
	// cron-triggered workflow, or anything else with no active conversation
	// to reply within.
	DefaultConversation() ConversationRef
}

// ToolProvider is implemented by a Messenger that offers extra tools beyond
// the built-in send_message — e.g. examples/messenger's SlackBot implements
// it for add_reaction/search_emoji, since reactions have no cross-platform
// equivalent and so don't belong on the Messenger interface itself.
// Optional: a Messenger that doesn't implement this just contributes
// send_message, same as before this existed.
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

type workflowIDCtxKey struct{}

// WithWorkflowID attaches the id of the workflow this run either IS (see
// runWorkflow) or traces back to (a reply in a thread whose root message a
// Messenger identified as workflow-tagged — e.g. Slack message metadata,
// see InboundMessage.WorkflowID) — same reasoning as WithMessageID/
// WithThreadID, so a Messenger's Send implementation and a MemoryKeyFunc
// can both read it off ctx without a dedicated parameter everywhere.
func WithWorkflowID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, workflowIDCtxKey{}, id)
}

// WorkflowIDFromContext returns the id WithWorkflowID attached, if any. ok
// is false both when nothing was attached and when an empty id was.
func WorkflowIDFromContext(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(workflowIDCtxKey{}).(string)
	return id, id != ""
}

type delegatorCtxKey struct{}

// WithDelegator attaches the delegating agent's own id to ctx, right before
// a delegate_to_<target> tool call hands ctx off to the target agent's
// runDelegatedTask — same reasoning as WithWorkflowID: a persistent Memory
// implementation that wants to record which agent's own conversation
// triggered a delegated run has no other way to learn that, since
// ConversationRef/ThreadID/WorkflowID are all agent-agnostic on their own.
func WithDelegator(ctx context.Context, agentID string) context.Context {
	return context.WithValue(ctx, delegatorCtxKey{}, agentID)
}

// DelegatorFromContext returns the id WithDelegator attached, if any. ok is
// false both when nothing was attached (not a delegated call) and when an
// empty id was.
func DelegatorFromContext(ctx context.Context) (string, bool) {
	id, _ := ctx.Value(delegatorCtxKey{}).(string)
	return id, id != ""
}

// MemoryKeyFunc derives the memory.Memory key for the current run — the
// extension point Agent.RunLoop/runWorkflow/runDelegatedTask use instead of
// a fixed key, so anyone building on this framework can choose (or write)
// their own partitioning strategy: one shared thread for the whole agent,
// one per conversation, one per thread, one per workflow, or any custom
// combination. See Agent.SetMemoryKeyFunc.
type MemoryKeyFunc func(ctx context.Context, conv ConversationRef) string

// KeyByAgent always returns the same key — one shared memory thread for the
// entire agent, regardless of platform/conversation/thread. The default
// when no MemoryKeyFunc is configured.
func KeyByAgent() MemoryKeyFunc {
	return func(ctx context.Context, conv ConversationRef) string { return "owner" }
}

// KeyByConversation partitions by platform+chat — a DM and a channel each
// get independent memory, but everything within one channel (its main
// timeline and every thread in it) shares one bucket.
func KeyByConversation() MemoryKeyFunc {
	return func(ctx context.Context, conv ConversationRef) string {
		return conv.Platform + ":" + conv.ChatID
	}
}

// KeyByThread partitions by platform+chat+thread. Keys directly on
// ThreadID with no special-casing: a message that starts a new thread has
// ThreadID == its own MessageID (every Messenger's documented ThreadID
// contract — see InboundMessage.ThreadID), so its own memory turn is
// already filed under that same value from the start, and a later reply
// inside that thread — whose ThreadID points back at the same root —
// resolves to the identical key.
//
// DMs (ConversationRef.DirectMessage == true) are the one exception:
// a DM conversation is inherently flat — a new DM gets no thread, and
// a thread reply is just UI sugar on the same 1:1 conversation — so
// the thread component is always dropped, collapsing every exchange in
// a DM to a single memory bucket regardless of whether the human used
// their client's "reply in thread" affordance.
func KeyByThread() MemoryKeyFunc {
	return func(ctx context.Context, conv ConversationRef) string {
		if conv.DirectMessage {
			return conv.Platform + ":" + conv.ChatID + ":"
		}
		threadID, _ := ThreadIDFromContext(ctx)
		return conv.Platform + ":" + conv.ChatID + ":" + threadID
	}
}

// KeyByWorkflow wraps another strategy, overriding it with a dedicated,
// stable per-workflow key whenever WorkflowIDFromContext is set — either
// this run IS a workflow run, or it's a reply that traced back to one —
// falling back to fallback otherwise. The workflow key is intentionally
// not shaped like fallback's own keys (no conv/thread component at all):
// it's meant to be one ongoing bucket across every run of that workflow,
// not scoped to any single conversation or thread.
func KeyByWorkflow(fallback MemoryKeyFunc) MemoryKeyFunc {
	return func(ctx context.Context, conv ConversationRef) string {
		if workflowID, ok := WorkflowIDFromContext(ctx); ok {
			return "workflow:" + workflowID
		}
		return fallback(ctx, conv)
	}
}
