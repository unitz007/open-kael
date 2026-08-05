package triggers

type TriggerType string

const (
	// CronTriggerType: Value holds a string cron expression, e.g. "0 9 * * *".
	CronTriggerType TriggerType = "trigger.cron"
	// WebhookTriggerType: Value holds a webhook.Source (see the webhook
	// package) — a live object with Path/Verify/Decode methods, not just
	// data, since receiving a webhook needs real verification/decoding
	// logic that differs per sender.
	WebhookTriggerType TriggerType = "trigger.webhook"
	// EventTriggerType: Value holds whatever an event-bus trigger ends up
	// needing — not yet defined, no current implementation.
	EventTriggerType TriggerType = "trigger.event"
)

// Trigger declares when/how a workflow runs. Value is deliberately typed as
// any rather than growing a new named field per TriggerType — Agent.Start
// type-asserts Value against whatever concrete type the Type constant
// above documents, so adding a new trigger type never requires touching
// this struct again.
type Trigger struct {
	Type  TriggerType
	Value any
}
