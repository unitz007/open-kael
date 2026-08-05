# Kael

Kael is a Go library for building LLM agents. Give an agent a system prompt, some tools, and optionally a messenger to talk over, a memory store, and identities to act as on external systems, and Kael handles the rest: the tool-calling loop, running scheduled workflows, and delegating tasks between agents.

This repo is just the platform: the agent loop, the messaging/memory/identity interfaces, Slack and Telegram messenger implementations, and an OpenAI-compatible LLM client. It ships no `Identity` implementations and no example business logic beyond `examples/` — you bring your own agents, tools, and identities by importing this module.

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

This registers two agents on a `Runtime` and launches it: an assistant and a research specialist reachable only via delegation. See [examples/basic/main.go](examples/basic/main.go) and [examples/researchspecialist/agent.go](examples/researchspecialist/agent.go).

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

## The loop, and what sits around it

At the center of an agent is `runLoopFrom`: given a transcript and a toolset, it calls the LLM with `tool_choice: "required"` (every response must call something — no plain-content replies), executes whatever tool comes back, and repeats, up to a fixed iteration cap, until something calls `end_loop`. A few things about that loop are true regardless of who's calling it:

- A tool-less response doesn't get accepted as an answer. It gets nudged — "that had no tool call, try again" — because silently accepting freeform text is exactly how an *unverified* answer (a fact the model invented instead of actually looking up) would sail straight through.
- A response with an unreasonable number of tool calls (a decoding glitch some models fall into — dozens of copies of the same call in one shot) gets discarded wholesale rather than executed.
- A tool already called once with the same arguments can't be called again — stops a model from re-sending, re-triggering, or re-delegating something that already succeeded.

None of that logic knows or cares whether it's running a real conversation, a workflow, or a delegated task. What *does* differ by caller is the transcript and toolset handed in — and that's decided by which of two entry points is used.

### Two ways into the loop, not one with a flag

**`RunLoop(ctx, conv, userPrompt)`** is for actual conversations. It loads that conversation's prior turns from memory, builds a toolset that includes `send_message` (if a messenger's registered), every workflow the agent owns, and a `delegate_to_<id>` tool for every sibling agent on the same `Runtime`. Once the loop finishes, it delivers the answer itself — `end_loop`'s final message goes to the messenger automatically, so a reply doesn't depend on the model remembering to call `send_message` along the way. (It still can, for something like an interim "still working on it" — the auto-delivery just backs off if `send_message` already succeeded, so nothing goes out twice.)

**`runDelegatedTask(ctx, task)`** is for when one agent hands work to another. It's not `RunLoop` with something disabled — it's a separate method with a structurally narrower toolset: the agent's own tools and its own workflows, and nothing else. No `ConversationRef`, because delegation isn't a conversation. No memory, because a delegated call is a one-off subroutine invocation, not a thread with history. No `send_message`, because there's no human on the other end to send it to. No further delegation, because a delegated agent handing the task off again is how you get a cycle two agents' prompts could walk into on their own. `delegate_to_<id>` calls this directly and blocks until it returns, folding the result straight into the delegator's own transcript.

A workflow, wherever it's triggered from, runs the same way: a *fresh* transcript scoped to that workflow's own system prompt, nested one level into the same `runLoopFrom` engine — never with access to other workflows or to delegation, so nesting never goes past one level in either direction.

## Talking to the outside world

`messaging.Messenger` is the interface a platform adapter implements — `Send`, `Listen`, `Platform`, `DefaultConversation`. This module ships Slack and Telegram implementations (`messaging/slack.go`, `messaging/telegram.go`); wire up additional ones (a CLI, Discord, whatever) against the same interface and pass them to `AddMessenger`. An agent only gets `send_message` in its toolset once something's actually been registered — no messenger, no tool, rather than a tool that's guaranteed to fail the moment it's called.

`ConversationRef{Platform, ChatID}` is how a reply gets routed back to the right place; `messaging.WithConversation`/`ConversationFromContext` thread the active one through `ctx` so a tool handler built with no knowledge of any specific conversation (or even of its own agent, in a workflow's case) can still find out where to reply.

## Identity — acting as someone on an external system

`identity.Identity` is deliberately bare:

```go
type Identity interface {
    Provider() string                         // "github", "aws", ...
    ActingAs() string                          // the account/username/role this identity presents as
    Token(ctx context.Context) (string, error) // a currently-valid credential, refreshed internally if needed
}
```

Unlike `Messenger`, there's no common action shape across arbitrary providers — opening a GitHub PR and deploying to AWS share nothing — so `Identity` doesn't define any actions itself; its only job is declaring who an agent is and handing back a currently-valid credential. `Token` is a method, not a field, so an implementation can transparently mint/refresh short-lived credentials instead of a caller ever reading a value that's gone stale.

`agent.IdentifyAs(id)` registers one, keyed by `Provider()`. Tools that need it call `identity.FromContext(ctx, "github")` — resolved through `ctx`, the same way `ConversationRef` is, since a workflow's own tools are built before the agent that will eventually own them even exists. A delegated call correctly resolves against the *target* agent's identities, not the delegator's, because identities are threaded onto `ctx` once, at the top of `runLoopFrom`.

## Memory

```go
type Memory interface {
    History(id string) []llm.Message
    Append(id string, messages ...llm.Message)
}
```

Two implementations ship: `memory.NewInMemoryHistory()` (per-process, lost on restart) and `memory.NewFileHistory(path)` (JSON-file-backed, atomic write-then-rename, survives a restart). Both trim history to a fixed cap per id. What `id` means is entirely up to the caller — nothing in this package assumes it's a conversation, a user, or anything else.

## Workflows

```go
type Workflow struct {
    ID, Name, Description, SystemPrompt string
    Trigger triggers.Trigger // cron, webhook, or event — value is trigger-specific (e.g. a cron expression)
    Tools   map[string]*tools.ToolSpec
}
```

A workflow is exposed to its owning agent as a single tool the model can call — `run_<id>` — which runs the workflow's own toolset through a fresh, nested loop and folds the result back into the caller's transcript.

## Tools

```go
tool := tools.NewToolBuilder("get_now_playing", "Gets movies currently in theaters").
    Parameter("region", "string", "ISO country code", false).
    Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
        return myClient.NowPlaying(), nil
    }).
    Build()

a.AddTool(tool)
```

## The LLM client

`llm.LLM` is a one-method interface (`Call(messages, tools) (*Response, error)`), so any chat-completions-shaped provider can back an agent. `llm/openai` ships an OpenAI-compatible client:

```go
openai.NewClient(model string, enableReasoning bool) llm.LLM
```

`enableReasoning` exists because a reasoning-capable model was observed writing a completely correct plan into its `reasoning` output — "I need to call `get_secret_code`" — and then the response would just end (`finish_reason: "stop"`) without the tool call ever actually landing. Not truncation, not confusion about what to do — the handoff from reasoning to the actual structured tool-call output just didn't happen. Passing `false` requests the underlying `reasoning: {enabled: false}` parameter and sidesteps it; if it resurfaces anyway, the loop's retry logs the real `finish_reason` and `reasoning` content so it's diagnosable instead of a silent, empty response.

## What's here

```text
agent/       the loop, RunLoop, runDelegatedTask, workflows-as-tools, delegation, built-in send_message/end_loop
runtime/     agent registry, shared event bus, Launch
messaging/   Messenger interface, ConversationRef, ctx helpers, Slack and Telegram implementations
identity/    Identity interface, ctx helpers — no implementations
memory/      Memory interface, InMemoryHistory, FileHistory
workflow/    Workflow struct
triggers/    TriggerType, Trigger
tools/       ToolSpec and its builder
llm/, llm/openai/  provider-agnostic types + an OpenAI-compatible client
events/      a pub/sub event bus
human/       a placeholder type, not yet used by anything in this module
examples/    a runnable two-agent example (examples/basic) and a delegation-only demo agent (examples/researchspecialist)
```

## What isn't finished

- **Cron doesn't actually fire workflows.** `Agent.Start` schedules the job and logs when it triggers, but the scheduled task only publishes an event — it never runs the loop. A cron-triggered workflow only executes today when something (a user, or the model's own reasoning) invokes it as a tool mid-conversation. Webhook triggers don't have this problem — `Agent.HandleWebhookEvent(ctx, eventKey, userTrigger)` runs the matching workflow directly; wire it to whatever's receiving the actual HTTP webhook.
- **One channel, one agent.** An agent needs its own `Messenger` instance to be reachable directly — there's no router letting several agents share one channel with a human choosing between them.
- `Runtime.Launch` starts agents fire-and-forget, no coordinated shutdown.
- No test coverage yet for the loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) — these have been manually verified during development but aren't pinned down by tests.
