package triggers

type TriggerType string

const (
	CronTriggerType    TriggerType = "trigger.cron"
	WebhookTriggerType TriggerType = "trigger.webhook"
	EventTriggerType   TriggerType = "trigger.event"
)

type Trigger struct {
	Type  TriggerType
	Value string
}
