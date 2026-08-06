// A starter reference showing every core piece together: a custom Identity,
// a custom Memory, a cron-triggered workflow, a webhook-triggered workflow,
// and a Messenger (Slack, via examples/messenger — swap in
// messenger.NewTelegramBot for Telegram instead). Copy this whole directory
// as the starting point for a real project; nothing here is meant to be
// imported as a dependency.
//
// Requires LLM_API_KEY and LLM_BASE_URL. Everything else (SLACK_BOT_TOKEN/
// SLACK_APP_TOKEN/SLACK_CHANNEL_ID, EXAMPLE_API_TOKEN, EXAMPLE_WEBHOOK_SECRET)
// is optional — the agent still runs without them, just with less wired up.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/examples/messenger"
	"github.com/unitz007/kael/llm/openai"
	"github.com/unitz007/kael/runtime"
	"github.com/unitz007/kael/triggers"
	"github.com/unitz007/kael/workflow"
)

func main() {
	a := agent.NewAgent(
		"starter",
		"Starter Agent",
		"A minimal reference agent showing identity, triggers, and messaging together.",
		"You are a helpful assistant.",
		openai.NewClient("z-ai/glm-5.2", false))

	// Identity — see identity.go. whoAmITool resolves it via
	// identity.FromContext, not by closing over the value above; that's
	// what makes it resolve correctly even from inside a workflow's own
	// nested toolset.
	a.IdentifyAs(newExampleIdentity())
	a.AddTool(whoAmITool())

	// Memory — NewAgent's own default is already a bare in-memory
	// implementation; SetMemory here is only to make the choice explicit.
	// Swap in FileHistory (memory_file.go) for persistence across restarts,
	// or write your own against memory.Memory for a real database.
	a.SetMemory(NewInMemoryHistory())

	// Messenger — Slack here. NewSlackBot never errors even with empty env
	// vars; it just fails the first time it actually tries to connect, so
	// this is safe to leave wired up even if you haven't set the Slack env
	// vars yet.
	a.AddMessenger(messenger.NewSlackBot("SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_CHANNEL_ID"))

	// A cron-triggered workflow — fires on its own schedule, no user
	// message involved. See kael-platform's README ("Triggers") for the
	// exact mechanics of what runs when this fires.
	dailyReportWorkflow := workflow.Workflow{
		ID:           "daily_report_wf",
		Name:         "Daily Report",
		Description:  "A scheduled workflow — replace with whatever should run on its own.",
		SystemPrompt: "Summarize something useful for the day and send it.",
		Trigger: triggers.Trigger{
			Type:  triggers.CronTriggerType,
			Value: "0 9 * * *", // daily, 9am
		},
	}
	a.AddWorkflow(&dailyReportWorkflow)

	// A webhook-triggered workflow — see webhook_source.go for the
	// webhook.Source implementation. Fires whenever a verified POST hits
	// its Path() (mounted below via rt.WebhookHandler()).
	onWebhookWorkflow := workflow.Workflow{
		ID:           "on_webhook_wf",
		Name:         "On Webhook",
		Description:  "Fires when a verified request hits /webhooks/example.",
		SystemPrompt: "React to whatever came in through the webhook.",
		Trigger: triggers.Trigger{
			Type:  triggers.WebhookTriggerType,
			Value: newExampleWebhookSource(),
		},
	}
	a.AddWorkflow(&onWebhookWorkflow)

	rt := runtime.NewRuntime()
	rt.RegisterAgent(a)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The library never opens a port itself — mounting rt.WebhookHandler()
	// on your own server is the one thing this file has to do for
	// webhook-triggered workflows to actually be reachable.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/webhooks/", rt.WebhookHandler())
		if err := http.ListenAndServe(":8080", mux); err != nil {
			log.Println("http server error:", err)
		}
	}()

	log.Println("starter example running — press Ctrl+C to stop")
	if err := rt.Launch(ctx); err != nil {
		log.Fatal("error launching runtime:", err)
	}
}
