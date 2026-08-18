# Kael

Kael is a Go library providing the runtime for LLM agents: a tool-calling loop, multi-agent delegation, scheduled and webhook-triggered workflows, and a mechanism for holding a sensitive tool call behind a human's approval before it runs. It defines the interfaces for messaging, memory, and identity, but ships no implementations — those are left to the application.

This repo is just the platform — no application code, no specific agents. Copyable reference implementations (`Identity`, `Memory`, `Messenger`, `webhook.Source`, and an OpenAI-compatible LLM client) live under [`examples/`](https://github.com/unitz007/open-kael/tree/main/examples) — see `examples/starter` for identity, memory, a cron workflow, a webhook workflow, and a messenger all together in one agent. Copy what fits, or bring your own; you build agents, tools, and identities by importing this module.

## Install

```bash
go get github.com/unitz007/kael
```

## Quickstart

```bash
cd examples/basic
export LLM_API_KEY="your-key"
export LLM_BASE_URL="https://openrouter.ai"
go run .
```

This registers two agents on a `Runtime` and launches it: an assistant and a research specialist reachable only via delegation. See [`examples/basic/main.go`](https://github.com/unitz007/open-kael/blob/main/examples/basic/main.go) and [`examples/researchspecialist/agent.go`](https://github.com/unitz007/open-kael/blob/main/examples/researchspecialist/agent.go).

For a single agent showing every piece together — a custom `Identity`, a cron-triggered workflow, a webhook-triggered workflow, and a `Messenger` — see [`examples/starter`](https://github.com/unitz007/open-kael/tree/main/examples/starter) instead. It's meant to be copied wholesale as the starting point for a real project.

## Building an agent

```go
a := agent.NewAgent(id, name, description, identityPrompt, llmClient)
a.AddTool(myTool)
a.AddWorkflow(myWorkflow)
a.AddMessenger(myMessenger)
a.IdentifyAs(myIdentity)

rt := runtime.NewRuntime()
rt.RegisterAgent(a)
rt.Launch(ctx) // blocks until ctx is cancelled
```

Registration on a `Runtime` is what makes agents visible to each other — an agent built but never registered has no delegation siblings, since `delegate_to_<id>` tools are generated from the registry, not from anything the agent holds itself.

Continue with the [Guide](guide/agents.md) for how each piece works, or jump straight to [Architecture](architecture/system.md) for the internals.

## What's here

```text
agent/       the loop, RunLoop, runWorkflow, runDelegatedTask, callTool (approval gating), workflows-as-tools,
             delegation + depth guard, built-in send_message/end_loop, panic recovery at every goroutine entry point
runtime/     agent registry, shared event bus, shared webhook mux + WebhookHandler(), Launch
messaging/   Messenger, ToolProvider, and ApprovalMessenger interfaces, ConversationRef, ctx helpers — no implementations
webhook/     Source interface (the webhook counterpart to Messenger), VerifyHMACSHA256 — no implementations
identity/    Identity interface, ctx helpers — no implementations
rag/         Retriever/Indexer interfaces, ctx helpers — no implementations
memory/      Memory interface — no implementations
workflow/    Workflow struct (Trigger, AllowDelegation, Iteration, Tools)
triggers/    TriggerType, Trigger
tools/       ToolSpec and its builder
llm/         provider-agnostic types (Message, Response, the LLM interface) — no implementations
events/      a pub/sub event bus
human/       Human{Name,Location,Timezone,Notes} + FromEnv() — read once inside agent.NewAgent
             (HUMAN_NAME/HUMAN_LOCATION/HUMAN_TIMEZONE/HUMAN_NOTES), nil if HUMAN_NAME is unset
examples/    starter (identity + memory + cron + webhook + messenger, copyable), basic (two-agent delegation demo),
             researchspecialist (delegation-only demo agent), messenger (Slack/Telegram Messenger reference),
             llm/openai (OpenAI-compatible chat-completions client reference)
```

## What isn't finished

- **One channel, one agent.** An agent needs its own `Messenger` instance to be reachable directly — there's no router letting several agents share one channel with a human choosing between them.
- **A delegated agent's use of `send_message`/reaction tools depends on the model choosing to.** The tools are offered, but nothing forces a delegate to call them — in practice, models observed during development reliably skip them on a simple, single-answer task and just rely on `end_loop`'s relay instead. Not a bug, just worth knowing if you're depending on it for something visible to the user.
- `Runtime.Launch` starts agents fire-and-forget, no coordinated shutdown.
- No test coverage yet for the loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) — these have been manually verified during development but aren't pinned down by tests.

See [Known Limitations](reference/known-limitations.md) and [Known Issues](architecture/known-issues.md) for the fuller lists.
