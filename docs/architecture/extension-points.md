# Extension Points

## Adding a tool

```go
tool := tools.NewToolBuilder("my_tool", "Does something").
	Parameter("param1", "string", "Description", true).
	Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
		// unwrap args as shown in Tools
		return result, nil
	}).
	Build()

a.AddTool(tool) // agent-level, or add to a Workflow.Tools map for a workflow-scoped one
```

## Adding an agent

```go
func MyAgent() *agent.Agent {
	a := agent.NewAgent("my_agent", "My Agent", "description", "You are...", openai.NewClient("model-name", false))
	a.AddTool(myTool)
	a.AddWorkflow(&myWorkflow)
	a.AddMessenger(messaging.NewTelegramBot()) // only if it should be directly reachable
	a.IdentifyAs(myIdentity)                   // only if it needs to act on an external system
	return a
}
```

Register with `rt.RegisterAgent(MyAgent())` — that's what makes it visible to (and able to delegate with) the other agents on the same `Runtime`.

## Adding a Messenger

```go
type MyBot struct{ /* ... */ }

func (b *MyBot) Platform() string { return "mychannel" }
func (b *MyBot) Send(ctx context.Context, conv messaging.ConversationRef, text string) error { /* ... */ }
func (b *MyBot) Listen(ctx context.Context, onMessage func(messaging.InboundMessage)) error { /* ... */ }
func (b *MyBot) DefaultConversation() messaging.ConversationRef { /* ... */ }
```

Anything satisfying `messaging.Messenger` works with `AddMessenger` — no other code needs to change. Only one agent (really, one process) should own a given underlying connection/bot token — there's no shared-listener/router mechanism for multiple agents to field messages from one channel. Optionally implement `ToolProvider` and/or `ApprovalMessenger` too — see [Messaging](../guide/messaging.md) and [Approval-Gated Tools](../guide/approval.md).

## Adding an Identity

```go
type MyIdentity struct{ /* credentials */ }

func (i *MyIdentity) Provider() string { return "myservice" }
func (i *MyIdentity) ActingAs() string { return "the-account-name" }
func (i *MyIdentity) Token(ctx context.Context) (string, error) { /* mint/refresh, return current token */ }
```

Register with `agent.IdentifyAs(myIdentity)`; a tool resolves it with `identity.FromContext(ctx, "myservice")`. See [Identity](../guide/identity.md).
