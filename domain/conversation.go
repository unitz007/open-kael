package domain

import "context"

// ConversationRef identifies a specific destination a message was received
// from, or should be sent to. ChatID is whatever that provider uses to address
// a destination — a channel, group, or user. IdentityID, when set, pins the
// reply to the exact Identity (bot/app) that received the inbound message —
// important when an agent has more than one Identity for the same service
// (e.g. two Telegram bots). Provider is kept for backward compat.
type ConversationRef struct {
	Provider   string
	ChatID     string
	ThreadID   string // optional; platform thread/topic ID (e.g. Telegram supergroup topic, Slack thread). Empty on platforms without threads.
	IdentityID string // set by Listener implementations; identifies which bot received the message
	UserID     string // optional; resolved by the host from MessengerChannel before HandleTurn
}

// InboundMessage is what a Listener (runtime host, not this package — see
// the framework/implementation split in messenger.go) hands off on a new
// message: the receiving counterpart to ActionSendMessage's outbound
// contract.
type InboundMessage struct {
	Conversation ConversationRef
	Text         string
	MessageID    string
	ThreadID     string
}

type ctxKey int

const (
	conversationCtxKey ctxKey = iota
	approvalRequesterCtxKey
)

// WithConversation/ConversationFromContext thread the active conversation
// through ctx rather than every call site passing it explicitly — mirrors
// kael-platform's messaging.WithConversation. HydrateTool's approval-gate
// wrapping (hydrate.go) is the first reader: a tool's Invoke closure needs
// to know which conversation to request approval in without every
// Executor's Execute signature carrying one.
func WithConversation(ctx context.Context, conv ConversationRef) context.Context {
	return context.WithValue(ctx, conversationCtxKey, conv)
}

func ConversationFromContext(ctx context.Context) (ConversationRef, bool) {
	conv, ok := ctx.Value(conversationCtxKey).(ConversationRef)
	return conv, ok
}

// ApprovalRequester is what a runtime host attaches to ctx (see
// WithApprovalRequester) so HydrateTool's approval-gate wrapping can ask a
// human before running a RequiresApproval tool, without HydrateTool itself
// needing to know which messenger or UI is asking. Mirrors kael-platform's
// messaging.ApprovalMessenger — a run that can't produce one for a
// RequiresApproval tool fails outright, never a silent unapproved run.
type ApprovalRequester interface {
	RequestApproval(ctx context.Context, conv ConversationRef, prompt string, timeoutSeconds int) (approved bool, err error)
}

func WithApprovalRequester(ctx context.Context, r ApprovalRequester) context.Context {
	return context.WithValue(ctx, approvalRequesterCtxKey, r)
}

func ApprovalRequesterFromContext(ctx context.Context) (ApprovalRequester, bool) {
	r, ok := ctx.Value(approvalRequesterCtxKey).(ApprovalRequester)
	return r, ok
}

// InteractiveMessenger is the provider-specific sliver a real,
// multi-tenant-safe ApprovalRequester needs — posting a prompt with some
// approve/reject affordance, and updating it once resolved. Deliberately
// narrower than ApprovalRequester itself: the correlation-ID/wait/timeout
// orchestration around this is entirely provider-agnostic (see
// runtime.MessengerApprovalRequester, the framework's real shipped
// implementation built on top of this interface) — only the two calls
// below differ per provider (Slack Block Kit buttons, Telegram inline
// keyboards, ...).
//
// integration is explicit on both methods, same reasoning as
// Executor.Execute: one Messenger instance is shared across every
// connected Integration of that provider (every tenant's own Slack
// workspace), so which credentials to use can't be baked in at
// construction time.
type InteractiveMessenger interface {
	// PostApprovalPrompt posts text as an interactive approve/reject
	// prompt into conv, tagged with token so a later button-click callback
	// (wired up by whoever owns this Messenger, not this interface) can
	// resolve the right pending wait. Returns an opaque messageID for a
	// later UpdateMessage call against the same message.
	// identity is the bot/app credential; connectionRef is the resolved
	// per-user token (from AppAuthorization) — empty for bot-only senders.
	PostApprovalPrompt(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, text string, token string) (messageID string, err error)
	// UpdateMessage overwrites messageID's text in place — used to show
	// the final approved/rejected/timed-out state.
	UpdateMessage(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, messageID string, text string) error
}
