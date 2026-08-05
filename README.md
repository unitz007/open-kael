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

**`RunLoop(ctx, conv, userPrompt)`** is for actual conversations. It loads that conversation's prior turns from memory, builds a toolset via `baseTools()` — the agent's own tools, `send_message` and any messenger-contributed extras (see [Talking to the outside world](#talking-to-the-outside-world)) once a messenger's registered — plus every workflow the agent owns and a `delegate_to_<id>` tool for every sibling agent on the same `Runtime`. Once the loop finishes, it delivers the answer itself — `end_loop`'s final message goes to the messenger automatically, so a reply doesn't depend on the model remembering to call `send_message` along the way. (It still can, for something like an interim "still working on it" — the auto-delivery just backs off if `send_message` already succeeded, so nothing goes out twice.)

**`runDelegatedTask(ctx, task)`** is for when one agent hands work to another. It's not `RunLoop` with something disabled — it's a separate method with its own toolset: `baseTools()` (same as above — send_message and messenger tools included) plus the agent's own workflows, minus `delegate_to_<id>`. No `ConversationRef` parameter, because delegation isn't a conversation — but `ctx` still carries whatever `ConversationRef`/message id the delegating call itself inherited, so `send_message`/`add_reaction` route through the *delegate's own* registered messenger against that same conversation if it chooses to use them (the delegate's system prompt makes clear this is optional, alongside — never instead of — its `end_loop` answer, which is what's actually relayed back to the delegator). No memory, because a delegated call is a one-off subroutine invocation, not a thread with history. No further delegation, because a delegated agent handing the task off again is how you get a cycle two agents' prompts could walk into on their own — enforced by a hard depth cap (`maxDelegationDepth`), not just by omission. `delegate_to_<id>` calls this directly and blocks until it returns, folding the result straight into the delegator's own transcript.

A workflow, wherever it's triggered from, runs the same way: a *fresh* transcript scoped to that workflow's own system prompt, nested one level into the same `runLoopFrom` engine, with no access to other workflows. It gets `delegate_to_<id>` tools too, but only if it opts in via `Workflow.AllowDelegation` — off by default, since most workflows are meant to be a bounded, single-purpose loop, not a dispatcher.

## Talking to the outside world

`messaging.Messenger` is the interface a platform adapter implements — `Send`, `Listen`, `Platform`, `DefaultConversation`. This module ships Slack and Telegram implementations (`messaging/slack.go`, `messaging/telegram.go`); wire up additional ones (a CLI, Discord, whatever) against the same interface and pass them to `AddMessenger`. An agent only gets `send_message` in its toolset once something's actually been registered — no messenger, no tool, rather than a tool that's guaranteed to fail the moment it's called.

A `Messenger` can optionally also implement `messaging.ToolProvider` (one method: `Tools() []*tools.ToolSpec`) to contribute platform-specific tools beyond `send_message` — `SlackBot` does this for `add_reaction`/`search_emoji`, since reactions have no cross-platform equivalent and don't belong on the `Messenger` interface itself. `Agent.baseTools()` checks every registered messenger for this and merges in whatever it returns, so any agent that registers a `SlackBot` inherits those tools automatically, with no per-agent wiring.

`ConversationRef{Platform, ChatID}` is how a reply gets routed back to the right place; `messaging.WithConversation`/`ConversationFromContext` thread the active one through `ctx` so a tool handler built with no knowledge of any specific conversation (or even of its own agent, in a workflow's case) can still find out where to reply.

## Triggers — cron, webhook, or event

`triggers.Trigger{Type, Value}` is deliberately typed with `Value any` rather than a named field per `TriggerType`, so a new trigger kind doesn't need a schema change: `Agent.Start()`'s type switch does the assertion per `TriggerType` constant.

- **`triggers.CronTriggerType`** — `Value` is a cron expression string. `Start()` schedules it with `gocron` and, on fire, runs the workflow through `runWorkflow` for real (not just an event publish) — `send_message` falls back to the messenger's `DefaultConversation()` since there's no active conversation to reply into.
- **`triggers.WebhookTriggerType`** — `Value` is a `webhook.Source` (package `webhook`), the webhook counterpart to `messaging.Messenger`: `Path()`, `Verify(body, header) bool`, `Decode(body, header) (userTrigger string, ok bool, err error)`. This module ships no implementations — GitHub, Stripe, or whatever else sends you a webhook each has its own payload shape and signature scheme; use `webhook.VerifyHMACSHA256` as the shared constant-time HMAC building block. `Start()` registers the source's `Path()` on the `Runtime`'s shared `http.ServeMux` (`Runtime.WebhookHandler()` — mount it on your own server), verifies and decodes each incoming request, and runs the matching workflow in a goroutine on success.
- **`triggers.EventTriggerType`** — reserved, not yet implemented.

## Delegation across workflows

`Workflow.AllowDelegation` opts a specific workflow into having `delegate_to_<sibling>` tools in its nested toolset — e.g. an issue-triage workflow whose whole job is deciding what to hand off to which agent. `Agent.runWorkflow` and `runDelegatedTask` both enforce a hard depth cap (`maxDelegationDepth`) regardless of this flag, so a misconfigured delegation cycle across multiple agents' workflows fails loudly with a depth-exceeded error rather than recursing forever.

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

## Retrieval — pulling indexed context into a run

`rag.Retriever`/`rag.Indexer` are the retrieval half of RAG: how a tool pulls relevant context (codebase snippets, docs, whatever's been indexed) into a run without it already being in the transcript. Same shape as `Identity` — this module defines the interface, ships no embedding model or vector store:

```go
type Retriever interface {
    Query(ctx context.Context, query string, topK int) ([]Result, error)
}
type Indexer interface {
    Index(ctx context.Context, docs []Document) error
    Delete(ctx context.Context, ids []string) error
}
```

Retriever and Indexer are kept separate (unlike `Memory`, which combines read/write) because they have a different trust boundary: almost anything that can query an index should be able to, but only a controlled ingestion path — not an arbitrary LLM tool call — should usually be able to add or remove what's in it.

`agent.AddRetriever(name, r)` registers one; tools call `rag.FromContext(ctx, name)`, threaded through `ctx` the same way identities are, for the same reason (a workflow's tools are built before the owning agent exists).

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
    Iteration       int                        // loop cap for this workflow's own nested run; 0 = agent's default
    Trigger         triggers.Trigger           // cron, webhook, or event — see Triggers above
    Tools           map[string]*tools.ToolSpec
    AllowDelegation bool                       // opt in to delegate_to_<sibling> tools — see Delegation above
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
agent/       the loop, RunLoop, runDelegatedTask, workflows-as-tools, delegation + depth guard, built-in send_message/end_loop
runtime/     agent registry, shared event bus, shared webhook mux + WebhookHandler(), Launch
messaging/   Messenger and ToolProvider interfaces, ConversationRef, ctx helpers, Slack and Telegram implementations
webhook/     Source interface (the webhook counterpart to Messenger), VerifyHMACSHA256 — no implementations
identity/    Identity interface, ctx helpers — no implementations
rag/         Retriever/Indexer interfaces, ctx helpers — no implementations
memory/      Memory interface, InMemoryHistory, FileHistory
workflow/    Workflow struct (Trigger, AllowDelegation, Iteration, Tools)
triggers/    TriggerType, Trigger
tools/       ToolSpec and its builder
llm/, llm/openai/  provider-agnostic types + an OpenAI-compatible client
events/      a pub/sub event bus
human/       a placeholder type, not yet used by anything in this module
examples/    a runnable two-agent example (examples/basic) and a delegation-only demo agent (examples/researchspecialist)
```

## What isn't finished

- **One channel, one agent.** An agent needs its own `Messenger` instance to be reachable directly — there's no router letting several agents share one channel with a human choosing between them.
- **A delegated agent's use of `send_message`/reaction tools depends on the model choosing to.** The tools are offered (see [Two ways into the loop](#two-ways-into-the-loop-not-one-with-a-flag)), but nothing forces a delegate to call them — in practice, models observed during development reliably skip them on a simple, single-answer task and just rely on `end_loop`'s relay instead. Not a bug, just worth knowing if you're depending on it for something visible to the user.
- `Runtime.Launch` starts agents fire-and-forget, no coordinated shutdown.
- No test coverage yet for the loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) — these have been manually verified during development but aren't pinned down by tests.
