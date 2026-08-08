# Kael Platform Documentation

This guide covers installing the module, running the bundled example, and how to extend Kael with new tools, agents, messaging platforms, and identities.

---

## Table of Contents

1. [Setup](#1-setup)
2. [Running the Example](#2-running-the-example)
3. [How Kael Handles a Message](#3-how-kael-handles-a-message)
4. [Creating Tools with ToolBuilder](#4-creating-tools-with-toolbuilder)
5. [Creating a New Agent](#5-creating-a-new-agent)
6. [Workflows](#6-workflows)
7. [Agent-to-Agent Delegation](#7-agent-to-agent-delegation)
8. [Messenger Interface](#8-messenger-interface)
9. [Identity Interface](#9-identity-interface)
10. [Memory](#10-memory)
11. [Troubleshooting](#11-troubleshooting)
12. [Known Limitations](#12-known-limitations)
13. [Related Docs](#13-related-docs)

---

## 1) Setup

### Prerequisites

- Go `1.26+`
- An OpenAI-compatible chat-completions endpoint (e.g. OpenRouter) — the only hard dependency; everything else (a messenger, an identity, a specific tool's own API) is opt-in per agent.

### Install

```bash
go get github.com/unitz007/kael
```

Or, working from a clone of this repo:

```bash
go mod tidy
```

There's no config file — every piece of this module that needs configuration reads it from environment variables at the point it's actually used, not centrally.

---

## 2) Running the Example

```bash
cd examples/basic
export LLM_API_KEY="your-key"
export LLM_BASE_URL="https://openrouter.ai"
go run .
```

This registers an assistant agent and a delegation-only research specialist ([examples/researchspecialist/agent.go](examples/researchspecialist/agent.go)) on a `Runtime` and launches it. Neither agent has a `Messenger` attached in the example, so nothing is reachable from outside the process yet — see [§8](#8-messenger-interface) to wire one up. `go build ./...` to just compile. `Ctrl+C` to stop.

**Run only one instance per bot token.** Telegram allows exactly one active `getUpdates` long-poll connection per bot token. A second instance — including one started from an IDE's own run button while a terminal instance is already up — doesn't error at startup; it silently competes for the same updates, and one of them stops receiving messages. See [Troubleshooting](#11-troubleshooting).

---

## 3) How Kael Handles a Message

```text
main()
  └─► runtime.NewRuntime()
        └─► RegisterAgent(agent)   # wires shared EventBus + AgentDirectory into each agent
        └─► Launch(ctx)
              └─► for each agent: go agent.Start(ctx)
                    └─► inbox listener (handles queued messages one at a time)
                    └─► one goroutine per registered Messenger, listening for inbound messages
                    └─► cron jobs scheduled for CronTriggerType workflows

Inbound message (any Messenger)
  └─► Messenger.Listen decodes it → Agent.EnqueueMessage(conv, text)
        └─► handleMessage → RunLoop(ctx, conv, text)
              └─► load this agent's memory (a single shared thread — see §10)
              └─► build toolset: agent's own tools (+ send_message if a messenger is
                  registered) + every workflow as a tool + every sibling agent as a
                  delegate_to_<id> tool
              └─► loop (max 6 iterations):
                    LLM.Call(messages, tools) — tool_choice is always "required"
                    execute whatever tool was called
                    repeat until end_loop is called
              └─► persist new turns back to memory
              └─► deliver the final answer via the messenger, unless send_message
                  already handled it during the run
```

---

## 4) Creating Tools with ToolBuilder

### API Reference

```go
tools.NewToolBuilder(name, description string) *ToolSpecBuilder
    .Parameter(name, paramType, description string, isRequired bool) *ToolSpecBuilder  // repeatable
    .Handler(func(ctx context.Context, args json.RawMessage) (any, error)) *ToolSpecBuilder
    .Build() *ToolSpec
```

### Reading Arguments

Tool arguments arrive **double-encoded**: `args` is a `json.RawMessage` whose contents are a JSON *string*, which itself contains the actual JSON object. Every tool handler in this module unwraps it the same two-step way — copy this pattern for a new tool:

```go
func MyTool() *tools.ToolSpec {
	return tools.NewToolBuilder("my_tool", "Does something useful").
		Parameter("message", "string", "The message to process", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}

			var input struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			// ... do the work ...
			return "done", nil
		}).
		Build()
}
```

### Example: Tool Without Parameters

```go
func GetSecretCodeTool() *tools.ToolSpec {
	return tools.NewToolBuilder("get_secret_code", "Looks up the current secret code").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			return secretCode, nil
		}).
		Build()
}
```

### Attaching a Tool

**Agent-level** (available on every message the agent handles):

```go
a := agent.NewAgent(...)
a.AddTool(MyTool())
```

**Workflow-level** (only available while that workflow's own nested loop is running):

```go
myWorkflow := workflow.Workflow{
	// ...
	Tools: map[string]*tools.ToolSpec{
		"my_tool": MyTool(),
	},
}
```

### Repeat calls

There's no per-tool opt-in for this — `runLoopFrom` itself blocks any tool from being called a second time with the *exact same arguments* once it's already succeeded once, regardless of which tool it is. This is enough to stop a model from re-sending the same message or re-delegating the same task after success, without needing a tool author to declare anything.

---

## 5) Creating a New Agent

```go
package myagents

import (
	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/llm/openai"
	"github.com/unitz007/kael/messaging"
)

func MyAgent() *agent.Agent {
	a := agent.NewAgent(
		"my_agent",                                       // id — must be unique across all registered agents
		"My Agent",                                        // display name
		"Handles X for the user.",                          // description — shown to sibling agents deciding whether to delegate
		"You are a helpful assistant that focuses on X.",  // identity prompt
		openai.NewClient("model-name", false),
	)

	a.AddTool(MyTool())
	// a.AddWorkflow(&myWorkflow)
	// a.AddMessenger(messaging.NewTelegramBot()) — only if this agent should be
	// directly reachable by a human; omit it to make the agent reachable only
	// via delegation from another agent (see examples/researchspecialist).
	// a.IdentifyAs(myIdentity) — only if a tool needs to act as this agent on
	// some external system.

	return a
}
```

Register it on a `Runtime`:

```go
rt := runtime.NewRuntime()
rt.RegisterAgent(MyAgent())
rt.RegisterAgent(AnotherAgent())
rt.Launch(ctx)
```

Registering through the same `Runtime` is what makes agents visible to each other for delegation — an agent constructed but never registered has no siblings and gets no `delegate_to_*` tools.

**Only register one agent's `AddMessenger` per bot token/connection.** Each `Messenger` gets its own `Listen` goroutine per agent that registers it; two agents (or two process instances) using the same underlying bot token will silently compete for updates. See [Troubleshooting](#11-troubleshooting).

---

## 6) Workflows

A `Workflow` is a task definition with its own system prompt, optional cron trigger, and optional own tools:

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

Once attached, a workflow is automatically exposed to the agent's own loop as a callable tool named after its `ID` — the model can decide to invoke it mid-conversation (e.g. "give me today's summary now"), which runs a **fresh, nested** loop scoped to the workflow's own system prompt and tools (plus the agent's own tools, so it can still reply/finish).

**The cron `Trigger` is currently scheduled but not executed** — `Agent.Start` registers the job and logs when it fires, but the scheduled task doesn't call into the loop yet. Right now, a workflow only actually runs when something (a user, or the agent's own reasoning) explicitly invokes it as a tool.

---

## 7) Agent-to-Agent Delegation

Any two agents registered on the same `Runtime` can see each other automatically — no extra wiring needed beyond `RegisterAgent`. Each agent gets a `delegate_to_<sibling-id>` tool per sibling, whose description includes that sibling's own tools and workflows, so the model can judge fitness rather than guess.

```go
rt.RegisterAgent(MyAgent())
rt.RegisterAgent(researchspecialist.Agent())
```

With both registered, `MyAgent` can decide mid-conversation to call `delegate_to_research_specialist` with a task description; that runs the specialist's own full loop synchronously and folds its answer back into the caller's own reply.

A few deliberate constraints:

- **Delegation is synchronous** — the calling agent's loop blocks until the target finishes. There's no fire-and-forget agent-to-agent messaging today.
- **One level deep only** — a delegated-to agent can't itself delegate further. This prevents two agents' prompts both saying "if unsure, ask the other one" from creating an infinite back-and-forth.
- **A delegated agent never gets `send_message`**, even if it has its own messenger registered for unrelated direct use — its only valid answer channel during a delegated call is its own `end_loop`.
- **A delegated agent resolves its own identities, not the delegator's** — if the target has its own `IdentifyAs` registered, its tools see that during the delegated call.
- **An agent with no `Messenger` is only reachable via delegation** — that's the whole point of `examples/researchspecialist`: it has no `AddMessenger` call, so nothing outside another agent's `delegate_to_research_specialist` call can reach it directly.

If you want a human to be able to reach a specific agent directly without going through another agent's judgment, the current answer is to give that agent its own separate `Messenger`/bot token — there's no router/addressing layer yet for multiple agents to share one channel.

---

## 8) Messenger Interface

```go
type Messenger interface {
	Platform() string
	Send(ctx context.Context, conv ConversationRef, text string) error
	Listen(ctx context.Context, onMessage func(InboundMessage)) error
	DefaultConversation() ConversationRef
}
```

### Built-in implementations

**`messaging.NewTelegramBot()`** reads `TELEGRAM_TOKEN`/`TELEGRAM_CHAT_ID` and returns a `Messenger` that:

- Sends messages via the Telegram Bot API, converting markdown to Telegram-compatible HTML (bold, italic, code, links, ordered/unordered lists → bullets, thematic breaks → "———" since Telegram's HTML mode has no `<hr>`).
- Long-polls `getUpdates` for inbound text messages, logging Telegram's own error/description whenever a poll fails rather than silently treating a failure as "no new messages."

**`messaging.NewSlackBot()`** reads `SLACK_APP_TOKEN`/`SLACK_BOT_TOKEN` and returns a `Messenger` that:

- Sends messages via `chat.postMessage`.
- Listens over Socket Mode (`apps.connections.open` + a websocket connection), acking each envelope and reconnecting automatically on disconnect.

### Creating a custom Messenger

```go
package mymessaging

import (
	"context"

	"github.com/unitz007/kael/messaging"
)

type DiscordBot struct {
	Token string
}

func (d *DiscordBot) Platform() string { return "discord" }

func (d *DiscordBot) Send(ctx context.Context, conv messaging.ConversationRef, text string) error {
	// POST to the Discord API using conv.ChatID as the channel target
	return nil
}

func (d *DiscordBot) Listen(ctx context.Context, onMessage func(messaging.InboundMessage)) error {
	// gateway connection or webhook receiver; call onMessage for each inbound
	// message, return when ctx.Done()
	return nil
}

func (d *DiscordBot) DefaultConversation() messaging.ConversationRef {
	return messaging.ConversationRef{Platform: "discord", ChatID: "some-default-channel"}
}
```

Register it the same way as the built-ins: `agent.AddMessenger(&DiscordBot{...})`.

---

## 9) Identity Interface

```go
type Identity interface {
	Provider() string                          // "github", "aws", ...
	ActingAs() string                           // the account/username/role this identity presents as
	Token(ctx context.Context) (string, error)  // a currently-valid credential, refreshed internally if needed
}
```

Use this when a tool needs to act as a specific credentialed identity on an external system — not for the tool itself to define new actions (there's no `Actions()` on this interface), just to declare who the agent is and hand back a live credential.

```go
type MyServiceIdentity struct {
	apiKey string
}

func (i *MyServiceIdentity) Provider() string { return "myservice" }
func (i *MyServiceIdentity) ActingAs() string { return "kael-bot" }
func (i *MyServiceIdentity) Token(ctx context.Context) (string, error) {
	return i.apiKey, nil // or mint/refresh a short-lived one here
}
```

Register it on the agent:

```go
a.IdentifyAs(&MyServiceIdentity{apiKey: os.Getenv("MYSERVICE_API_KEY")})
```

Resolve it inside a tool's handler:

```go
func MyServiceTool() *tools.ToolSpec {
	return tools.NewToolBuilder("do_something_on_myservice", "...").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			id, ok := identity.FromContext(ctx, "myservice")
			if !ok {
				return nil, fmt.Errorf("no myservice identity configured for this agent")
			}
			token, err := id.Token(ctx)
			if err != nil {
				return nil, err
			}
			// use token to call the external API
			return result, nil
		}).
		Build()
}
```

This works even for a tool attached to a `Workflow`, built before any specific agent owns it — `identity.FromContext` resolves whichever agent's loop is actually running the tool at call time, not whichever agent the tool happened to be written for.

---

## 10) Memory

```go
type Memory interface {
	History(id string) []llm.Message
	Append(id string, messages ...llm.Message)
}
```

Two implementations ship: `memory.NewInMemoryHistory()` (process-local, lost on restart) and `memory.NewFileHistory(path)` (JSON-file-backed, survives a restart). `agent.NewAgent` wires every agent to a `FileHistory` at `data/memory/<id>.json` automatically, falling back to in-memory on any filesystem error.

**How memory gets partitioned is a configurable strategy, not a fixed rule.** `RunLoop`/`runWorkflow`/`runDelegatedTask` all derive their memory key via `Agent.SetMemoryKeyFunc(fn messaging.MemoryKeyFunc)` — unset, it defaults to `messaging.KeyByAgent()`, one shared thread for the whole agent regardless of platform/conversation/thread (the original single-owner behavior: an agent reachable on both Telegram and Slack remembers the same conversation across both, at the cost of nothing distinguishing *who* is messaging). `messaging.KeyByConversation()`/`KeyByThread()` partition by `ConversationRef`/Slack thread instead, and `KeyByWorkflow(fallback)` gives each workflow its own ongoing bucket across every run — including a reply that traces back to it via a Messenger-specific tagging mechanism (Slack: invisible message metadata read back via `conversations.replies`; see `messaging.WithWorkflowID`). Compose these, or write your own `MemoryKeyFunc` — e.g. per-user isolation, which nothing ships today (no user-id field exists in the inbound pipeline), is exactly the kind of thing a custom strategy is for.

---

## 11) Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| App exits immediately at startup | Missing `LLM_API_KEY` or `LLM_BASE_URL` | Set both environment variables |
| No reply to a message, no errors logged | A second process is using the same bot token/connection | Check for a duplicate process, kill it, restart one instance |
| `telegram getUpdates failed (code 409): Conflict...` | Same as above, for Telegram — surfaced explicitly in logs | Same fix |
| `⏱️...hit the N-iteration cap without calling end_loop` | The model never produced a valid final answer within 6 iterations | Usually a sign the request needs breaking down, or that a tool is failing repeatedly — check for `Tool execution error` lines just before it |
| `⚠️...returned plain text instead of a tool call` | The model responded without calling any tool — the loop nudges it to retry rather than accepting unverified content; not by itself an error, just a retry cost | If it happens repeatedly for the same kind of request, tighten that agent/workflow's prompt. If it persists even after a retry, check the logged `finish_reason`/`reasoning` — a reasoning-capable model may need `enableReasoning: false` (see `llm/openai.NewClient`) |
| `⚠️...response contained N tool calls in one turn` | The model's response degenerated into repeating the same tool call many times (a decoding glitch) — the whole batch is discarded automatically | Not actionable beyond noting which request triggered it; retry usually succeeds |
| `⚠️...tried to repeat ... — blocked` | The model tried to call the same tool with the same arguments twice after it already succeeded | Expected behavior, not a bug — if the model keeps needing this, its prompt may be unclear about when to call `end_loop` |
| "agent ... already exists in the registry" log | Duplicate `RegisterAgent` call with the same `Id` | Agent IDs must be unique |
| `send_message: could not resolve a target` / `no messenger registered` | The agent has no `Messenger` registered, or the active conversation's platform doesn't match any registered messenger | Call `AddMessenger` for that platform, or don't expose `send_message` to that agent at all |

### Debug tips

- Startup: `🎯kael started successfully with N agent(s).`
- Per-agent startup: `🤖{AgentName} started successfully`
- Message handling: `💬{AgentName}: handling message from ...` → `💬{AgentName}: finished handling message ... status=... iterations=N`
- Outbound reply: `📤{AgentName}: routing send_message to {platform}/{chatID}`
- Workflow trigger (cron): `🧨{WorkflowName} for {AgentName} Triggered` (note: currently doesn't execute — see [Known Limitations](#12-known-limitations))
- Delegation: `🤝{AgentName}: delegating to {Target}: "..."` → `🤝{AgentName}: {Target} finished (status): ...`

---

## 12) Known Limitations

- **Cron-triggered workflows don't execute yet** — only chat-triggered invocation (the model calling a workflow as a tool) actually runs one.
- **No graceful shutdown** — agent goroutines in `Runtime.Launch` are fire-and-forget, no `sync.WaitGroup`.
- **Double JSON unmarshal** — every tool handler receives a JSON string wrapper around its actual arguments (see [§4](#4-creating-tools-with-toolbuilder)).
- **Single trigger type implemented** — `WebhookTriggerType`/`EventTriggerType` are declared but have no handling in `Agent.Start`.
- **No shared-channel routing** — each agent needs its own `Messenger`/bot token to be directly reachable by a human; there's no way yet for multiple agents to field messages from one shared channel and have a human pick which one to address.
- **No built-in per-user memory partitioning preset** — see [§10](#10-memory); write a custom `MemoryKeyFunc` if you need it, not a fit for a multi-tenant deployment out of the box otherwise.
- **No test coverage** for the loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) yet — manually verified during development only.

---

## 13) Related Docs

| Document | Content |
|----------|---------|
| `README.md` | Quick start and overview |
| `TECHNICAL_DOC.md` | Deep architecture, data flows, known issues, file map |
