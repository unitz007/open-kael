# Kael Platform

Kael is a Go framework for building tool-using LLM agents. It provides the
runtime pieces that are useful across applications: agent loops, tool schemas,
workflow execution, delegation, messaging interfaces, identities, memory,
retrieval hooks, webhooks, approval gates, peer connections, and task queues.

This repository is the platform only. It does not ship Charles' personal agents
or production integrations. Those live in the sibling `Kael` app repository,
which imports this module as `github.com/unitz007/kael`.

## Install

```bash
go get github.com/unitz007/kael
```

## Minimal Agent

```go
package main

import (
    "context"

    "github.com/unitz007/kael/agent"
    "github.com/unitz007/kael/runtime"
)

func main() {
    llm := newLLMClient()

    assistant := agent.NewAgent(
        "assistant",
        "Assistant",
        "General-purpose assistant",
        "You are a careful assistant. Use tools when needed.",
        llm,
    )

    assistant.AddTool(newExampleTool())

    rt := runtime.NewRuntime()
    rt.RegisterAgent(assistant)
    rt.Launch(context.Background())
}
```

Registering an agent on a `Runtime` is what makes it visible to sibling agents
for delegation. An agent can also be used on its own, but it will not discover
other agents unless a directory is wired in.

## Core Concepts

### Agent

`agent.Agent` owns:

- an ID, name, description, and identity prompt
- one or more LLM providers, tried in priority order
- tools
- workflows
- messengers
- identities
- retrievers
- memory
- loop guardrails

The main constructor is:

```go
agent.NewAgent(id, name, description, identityPrompt string, llms ...llm.LLM)
```

### Tools

Tools are defined with `tools.ToolSpec` or the builder:

```go
tool := tools.NewToolBuilder("get_weather", "Gets weather for a city").
    Parameter("city", "string", "City name", true).
    Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
        return weatherResult, nil
    }).
    Build()
```

Handlers receive a `context.Context` that can carry the active conversation,
agent identities, retrievers, human details, and other runtime-scoped values.

### The Loop

The native agent loop receives a transcript and a toolset, calls the LLM, runs
tool calls, appends tool results, and repeats until `end_loop` is called or a
configured iteration limit is reached.

Important guardrails:

- Tool calls are required for each LLM response when the provider supports it.
- Tool-less responses are nudged instead of silently accepted.
- Repeated identical tool calls in one response are treated as likely decoding
  glitches and blocked past a threshold.
- Very large tool-call batches are capped.
- A tool already called with the same arguments in the same run is not called
  again.
- Panics inside agent work are recovered and logged at runtime goroutine
  boundaries.

### Workflows

A workflow is a named, tool-backed routine owned by an agent:

```go
workflow.Workflow{
    ID:           "daily_briefing",
    Name:         "Daily Briefing",
    Description:  "Prepares a morning update",
    SystemPrompt: "Prepare a concise briefing.",
    Trigger:      triggers.Trigger{Type: triggers.CronTriggerType, Value: "0 7 * * *"},
    Tools:        map[string]*tools.ToolSpec{...},
}
```

Workflows can be called by the owning agent as tools, or fired by triggers. A
workflow runs in a fresh nested transcript with its own system prompt and
workflow-specific toolset.

Delegation from workflows is opt-in:

```go
wf.AllowDelegation = true
```

### Triggers

The platform currently supports:

- `triggers.CronTriggerType`: cron expression string
- `triggers.WebhookTriggerType`: a `webhook.Source`
- `triggers.EventTriggerType`: reserved for future use

Cron workflows are scheduled by the agent. Webhook workflows register their
source path on the runtime's shared webhook mux.

### Webhooks

`webhook.Source` describes a webhook producer:

```go
type Source interface {
    Path() string
    Verify(body []byte, header http.Header) bool
    Decode(body []byte, header http.Header) (userTrigger string, ok bool, err error)
}
```

Mount runtime webhooks in your own HTTP server:

```go
mux.Handle("/webhooks/", rt.WebhookHandler())
```

Use `webhook.VerifyHMACSHA256` for constant-time HMAC verification when the
upstream service supports signed payloads.

### Messaging

The platform defines messaging interfaces but does not own any concrete
messenger account:

```go
type Messenger interface {
    Send(ctx context.Context, conv ConversationRef, text string) error
    Listen(ctx context.Context, handler Handler) error
    Platform() string
    DefaultConversation() ConversationRef
}
```

Register a messenger with:

```go
a.AddMessenger(myMessenger)
```

Once an agent has a messenger, the platform adds `send_message` to its toolset.
`end_loop` also auto-delivers the final answer for real conversations, so a
normal reply does not depend on the model remembering to call `send_message`.

`messaging.ConversationRef{Platform, ChatID}` and context helpers route replies
without hardcoding a platform into tool handlers.

### Messenger Tools

A messenger can implement `messaging.ToolProvider` to add platform-specific
tools. For example, a Slack implementation can contribute `add_reaction`
without forcing reactions onto every messenger interface.

### Approvals

Any tool can require human approval before its handler runs:

```go
tool := tools.NewToolBuilder("place_trade", "Places a real trade").
    RequireApproval(func(ctx context.Context, args json.RawMessage) string {
        return "Approve this trade?"
    }, 3*time.Minute).
    Handler(placeTrade).
    Build()
```

Approval is enforced in the agent's tool-call path. If the active messenger
does not implement `messaging.ApprovalMessenger`, the call fails rather than
running without approval.

### Delegation

Agents registered on the same runtime can delegate to one another through
generated `delegate_to_<agent_id>` tools.

A delegated task is not a normal conversation:

- it has no conversation history
- it cannot delegate again
- it returns its result to the delegating agent
- it may still use the target agent's messenger tools if the inherited context
  contains a conversation

The platform also supports external delegate targets through the
`agent.DelegateTarget` interface, so a runtime can hand work to a process that is
not a native in-process Kael agent.

### Peer Runtime

`runtime.Peer` lets two runtimes connect over a websocket and expose their agents
to each other as delegate targets. This is useful when one agent must live in a
different trust boundary, machine, or working directory.

If a `runtime.TaskQueue` is configured, tasks for a known-but-offline peer can
be queued and drained when that peer reconnects.

### Identity

Identities answer one question: who is this agent acting as for a provider?

```go
type Identity interface {
    Provider() string
    ActingAs() string
    Token(ctx context.Context) (string, error)
}
```

Register one with:

```go
a.IdentifyAs(githubIdentity)
```

Tools resolve identities from context:

```go
id, ok := identity.FromContext(ctx, "github")
```

`Token` is a method so implementations can refresh short-lived credentials
internally.

### Retrieval

The RAG interfaces are intentionally storage/model agnostic:

```go
type Retriever interface {
    Query(ctx context.Context, query string, topK int) ([]Result, error)
}

type Indexer interface {
    Index(ctx context.Context, docs []Document) error
    Delete(ctx context.Context, ids []string) error
}
```

Register retrievers on the agent and resolve them from tool contexts with
`rag.FromContext`.

### Memory

The memory interface is small:

```go
type Memory interface {
    History(ctx context.Context, id string) []llm.Message
    Append(ctx context.Context, id string, messages ...llm.Message)
}
```

The platform has a process-local default so simple agents work immediately. Use
your own database-backed memory for production systems that need durable
conversation history.

### Human Details

`human.Human` carries owner details such as name, location, timezone, and notes.
`runtime.SetHuman` propagates those details to registered agents. `human.FromEnv`
can load:

- `HUMAN_NAME`
- `HUMAN_LOCATION`
- `HUMAN_TIMEZONE`
- `HUMAN_NOTES`

## Runtime

`runtime.Runtime` owns:

- local agent registry
- shared event bus
- shared webhook mux
- peer connections
- optional task queue
- optional trigger state
- optional human details

Typical setup:

```go
rt := runtime.NewRuntime()
rt.SetHuman(human.FromEnv())
rt.SetTaskQueue(myQueue)
rt.SetTriggerState(myTriggerState)
rt.RegisterAgent(agentA)
rt.RegisterAgent(agentB)
rt.Launch(ctx)
```

`Runtime.Launch` starts registered agents. Your application still owns the HTTP
server, process lifecycle, secrets, and concrete integrations.

## Package Map

```text
agent/       agent loop, tool calling, workflows, approvals, delegation,
             guardrails, panic recovery, external delegate support
runtime/     runtime registry, shared webhook mux, event bus, peer transport,
             queued remote delegation
tools/       ToolSpec and builder
workflow/    workflow definition
triggers/    trigger types
messaging/   Messenger, ToolProvider, ApprovalMessenger, ConversationRef,
             context helpers
identity/    Identity interface and context helpers
memory/      Memory interface
rag/         Retriever and Indexer interfaces
webhook/     Source interface and HMAC verification helper
events/      pub/sub event bus
human/       owner context
llm/         provider-agnostic LLM request/response types
examples/    copyable starter/basic agents, messenger refs, OpenAI-compatible
             LLM client reference
docs/        mkdocs documentation
```

## Examples

The examples are intended as copyable references:

- `examples/starter`: one agent with identity, memory, messenger, cron workflow,
  and webhook workflow.
- `examples/basic`: small multi-agent delegation demo.
- `examples/researchspecialist`: specialist agent used by the basic example.
- `examples/messenger`: Slack and Telegram messenger reference implementations.
- `examples/llm/openai`: OpenAI-compatible chat completions client.

## Design Boundaries

Kael Platform deliberately avoids concrete provider implementations in the core
packages:

- no built-in Slack/Gmail/GitHub/Google/AWS clients
- no built-in vector database
- no production database memory
- no app-specific agents
- no owned HTTP port

That keeps the framework reusable. Applications bring their own integrations and
wire them through the platform interfaces.

## Testing

```bash
go test ./...
go vet ./...
```

## License

See `LICENSE`.
