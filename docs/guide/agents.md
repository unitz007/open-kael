# Building an Agent

```go
package myagents

import (
	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/examples/llm/openai"
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

!!! warning "One agent per bot token"
    Each `Messenger` gets its own `Listen` goroutine per agent that registers it; two agents (or two process instances) using the same underlying bot token will silently compete for updates. See [Troubleshooting](../reference/troubleshooting.md).

## Multiple LLM providers (fallback)

`NewAgent`'s last parameter is variadic (`llms ...llm.LLM`) — passing a single client works exactly as shown above, but an agent can also be given more than one provider, tried in order with automatic cooldown-based fallback if the first starts failing:

```go
a := agent.NewAgent(id, name, description, prompt,
	openai.NewClient("primary-model", false),
	openai.NewClient("fallback-model", false), // tried once the primary is judged down
)
```

This is how a production deployment typically avoids a single provider outage taking an agent down entirely — see [Environment Variables](../architecture/environment-variables.md) and any real agent constructor for the pattern of gating a fallback provider behind its own API-key environment variable.

## What `AddTool`/`AddWorkflow`/`AddMessenger`/`IdentifyAs` each do

| Call | Effect |
|------|--------|
| `AddTool(tool)` | Adds to `a.Tools` — unconditionally available on every call this agent handles (chat, workflow, or delegated), regardless of any other flag. See [Tools](tools.md). |
| `AddWorkflow(wf)` | Registers a workflow, exposed to the agent's own loop as a callable tool named after its `ID`, and scheduled/registered if it has a `CronTriggerType`/`WebhookTriggerType` trigger. See [Workflows & Triggers](workflows.md). |
| `AddMessenger(m)` | Registers a `Messenger` this agent can send/receive through. The *first* one registered becomes the resolve-order default for `send_message` when no active conversation is on `ctx`. See [Messaging](messaging.md). |
| `IdentifyAs(id)` | Registers an `Identity`, keyed by `Provider()`. A tool resolves it at call time via `identity.FromContext(ctx, provider)`. See [Identity](identity.md). |

An agent constructed but never passed to `Runtime.RegisterAgent` has no delegation siblings and, if it has a webhook-triggered workflow, no route mounted anywhere — registration is what actually wires it into the running system.
