package domain

type TriggerType string

const (
	// TriggerTypeManual is the implicit default — the Skill is invoked by the
	// agent's own loop during a conversation turn, not by an external signal.
	TriggerTypeManual TriggerType = "manual"

	// TriggerTypeCron fires a Skill on a time schedule.
	// Trigger.Value is a standard cron expression (e.g. "0 9 * * *").
	TriggerTypeCron TriggerType = "cron"

	// TriggerTypeEvent fires a Skill when a named event occurs — whether that
	// event arrived via an inbound HTTP POST from an external service (GitHub,
	// Stripe, Linear, …) or was emitted internally by this system (a user
	// connecting a channel, a turn completing, etc.). The two delivery paths
	// are an implementation detail of the event bus; the Skill only ever
	// sees the canonical event name.
	// Trigger.Value is the event name, e.g. "github.pull_request.opened",
	// "stripe.invoice.paid", "user.channel.connected".
	TriggerTypeEvent TriggerType = "event"
)

// Trigger says a Skill also fires on its own, outside of being invoked by the
// agent's conversational loop.
type Trigger struct {
	Type  TriggerType `json:"type"`
	Value string      `json:"value"` // cron expression or event name — meaning depends on Type

	// NotifyConversation is where this Skill's result should be posted once
	// it finishes — nil means fire-and-forget (result only logged). A
	// self-triggered Skill has no inbound conversation to reply into, so the
	// destination must be named explicitly.
	NotifyConversation *ConversationRef `json:"notifyConversation,omitempty"`
}
