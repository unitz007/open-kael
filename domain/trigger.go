package domain

type TriggerType string

const (
	TriggerTypeManual  TriggerType = "manual" // invoked directly — the implicit default when Trigger is nil
	TriggerTypeCron    TriggerType = "cron"
	TriggerTypeWebhook TriggerType = "webhook"
	TriggerTypeEvent   TriggerType = "event"
)

// Trigger says a Skill also fires on its own, outside of being invoked. A
// fresh, minimal shape — the same {Type, Value} idea as kael-platform's own
// triggers.Trigger, deliberately re-declared here rather than imported,
// since domain/ doesn't depend on any other kael-platform package.
type Trigger struct {
	Type  TriggerType `json:"type"`
	Value string      `json:"value"` // cron expression, webhook path, event name — meaning depends on Type
}
