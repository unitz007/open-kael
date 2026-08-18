# Workflows & Triggers

A `Workflow` is a task definition with its own system prompt, an optional trigger, and optional own tools:

```go
type Workflow struct {
	ID, Name, Description, SystemPrompt string
	Iteration                            int              // loop cap for this workflow's own nested run; 0 = agent's default
	Trigger                              triggers.Trigger // cron, webhook, or event
	Tools                                map[string]*tools.ToolSpec
	AllowDelegation                      bool // opt in to delegate_to_<sibling> tools — see Delegation
	MaxDuplicateToolCallsPerResponse      int  // 0 = agent's default
	MaxToolCallsPerResponse              int  // 0 = agent's default
	MaxDuplicateToolCallsPerResponseFunc func(ctx context.Context) (int, error) // dynamic alternative,
	MaxToolCallsPerResponseFunc          func(ctx context.Context) (int, error) // takes priority if set
}
```

```go
myWorkflow := workflow.Workflow{
	ID:           "daily_summary_wf",
	Name:         "Daily Summary Workflow",
	Description:  "Summarizes the day's activity and sends it to the user.",
	SystemPrompt: "You are a summarizer. Gather the day's data and send a concise summary.",
	Trigger: triggers.Trigger{
		Type:  triggers.CronTriggerType,
		Value: "0 20 * * *", // 8 PM daily
	},
	Tools: map[string]*tools.ToolSpec{
		"get_activity_log": GetActivityLogTool(),
	},
}

a.AddWorkflow(&myWorkflow)
```

## As a callable tool

Once attached, a workflow is automatically exposed to the agent's own loop as a callable tool named after its `ID` — the model can decide to invoke it mid-conversation (e.g. "give me today's summary now"), which runs a **fresh, nested** loop scoped to the workflow's own system prompt and tools (plus the agent's own tools, so it can still reply/finish). Nesting never goes past one level — a workflow's own toolset never includes another workflow, and only includes `delegate_to_<sibling>` tools if `AllowDelegation` is set.

## Triggers — cron, webhook, or event

`triggers.Trigger{Type, Value}` is deliberately typed with `Value any` rather than a named field per `TriggerType`, so a new trigger kind doesn't need a schema change — `Agent.Start()`'s type switch does the assertion per constant.

- **`triggers.CronTriggerType`** — `Value` is a cron expression string. `Start()` schedules it with `gocron` and, on fire, runs the workflow through `runWorkflow` for real — not just an event publish — with `send_message` falling back to the messenger's `DefaultConversation()` since there's no active conversation to reply into.
- **`triggers.WebhookTriggerType`** — `Value` is a `webhook.Source` (package `webhook`), the webhook counterpart to `messaging.Messenger`: `Path()`, `Verify(body, header) bool`, `Decode(body, header) (userTrigger string, ok bool, err error)`. This module ships no implementations — GitHub, Stripe, or whatever else sends you a webhook each has its own payload shape and signature scheme; use `webhook.VerifyHMACSHA256` as the shared constant-time HMAC building block. `Start()` registers the source's `Path()` on the `Runtime`'s shared `http.ServeMux` (`Runtime.WebhookHandler()` — mount it on your own server), verifies and decodes each incoming request, and runs the matching workflow in a goroutine on success.
- **`triggers.EventTriggerType`** — reserved, not yet implemented.

Both `CronTriggerType` and `WebhookTriggerType` fire on their own goroutine, wrapped in panic recovery — see [Reliability](../architecture/system.md#reliability).

## Delegation across workflows

`Workflow.AllowDelegation` opts a specific workflow into having `delegate_to_<sibling>` tools in its nested toolset — e.g. an issue-triage workflow whose whole job is deciding what to hand off to which agent. `Agent.runWorkflow` and `runDelegatedTask` both enforce a hard depth cap (`maxDelegationDepth`) regardless of this flag, so a misconfigured delegation cycle across multiple agents' workflows fails loudly with a depth-exceeded error rather than recursing forever. See [Delegation](delegation.md) for the mechanism itself.
