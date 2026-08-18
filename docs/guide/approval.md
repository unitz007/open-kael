# Approval-Gated Tools

A tool can require a human's approval before its `Handler` actually runs — declared on the tool itself, not on whatever calls it:

```go
tool := tools.NewToolBuilder("place_trade", "Places a real trade").
	Parameter("symbol", "string", "...", true).
	RequireApproval(func(ctx context.Context, args json.RawMessage) string {
		return "About to place a trade — approve?" // the approval prompt's text
	}, 3*time.Minute). // timeout — unanswered is treated as reject, never "keep waiting"
	Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
		return broker.PlaceTrade(...), nil // only runs once approved
	}).
	Build()
```

This is deliberately a **tool-definition-level flag**, not a separate messaging primitive or a wrapper the calling agent has to remember to apply — a tool that needs approval needs it everywhere it's reachable from (chat, a workflow, a delegated call) without three different call sites each having to know that.

## How it's enforced

Every tool call — from a real conversation, a workflow, or a delegated task — passes through `Agent.callTool`, the single interception point:

```go
func (a *Agent) callTool(ctx context.Context, tool *tools.ToolSpec, args json.RawMessage) (any, error) {
	if !tool.RequiresApproval {
		return tool.Handler(ctx, args)
	}
	conv, ok := a.DefaultConversation()
	// ... resolve the agent's own conversation, never an ambient one from ctx ...
	am, ok := m.(messaging.ApprovalMessenger)
	if !ok {
		return nil, fmt.Errorf("%s requires approval, but %s doesn't support interactive approval — refusing to run rather than executing unapproved", ...)
	}
	// ... block on am.RequestApproval up to tool.ApprovalTimeout (default 10 minutes) ...
}
```

- If `!tool.RequiresApproval`, `Handler` runs directly — zero overhead for every ordinary tool.
- Otherwise it resolves the agent's own `DefaultConversation()` — **deliberately not** whatever `ConversationRef` happens to be on `ctx`. During a delegated call, `ctx` carries the *delegating* agent's conversation, not the delegate's; resolving against ambient `ctx` would try to post an approval prompt into a conversation belonging to a different agent's messaging identity (a different bot token, likely failing outright — meaning no human ever sees the prompt). This was a real bug, fixed after being found.
- Requires the registered messenger to implement `messaging.ApprovalMessenger` — a messenger that doesn't makes the call **fail outright**, with a clear error. Never a silent bypass that runs the handler unapproved.
- Blocks on `RequestApproval` up to `ApprovalTimeout` (default 10 minutes if left zero). Declined or timed out returns an ordinary "not approved" tool result — `Handler` simply never runs. Approved runs `Handler` exactly as if `RequiresApproval` were false.

```go
type ApprovalMessenger interface {
	RequestApproval(ctx context.Context, conv ConversationRef, text string) (approved bool, err error)
}
```

## Implementing it for your own messenger

`examples/messenger`'s `SlackBot` implements `ApprovalMessenger` as a thin wrapper over its own pre-existing button-based `WaitForApproval` — copy that pattern:

```go
func (s *SlackBot) RequestApproval(ctx context.Context, conv messaging.ConversationRef, text string) (bool, error) {
	return s.WaitForApproval(ctx, conv, text)
}
```

Anything satisfying `ApprovalMessenger` works — a button UI isn't required; a messenger could just as well implement it as a reply-with-"yes" text prompt.

## Design note

This mechanism replaced an earlier design that bolted approval onto the messaging layer (a `needs_response` flag on `send_message`) — moving it to the tool definition itself means a tool's safety requirement travels with the tool everywhere it's reachable from, rather than depending on every call site remembering to gate it. See [Architecture § System Architecture](../architecture/system.md) for the full internals, including why this specific composition (`callTool` as the single interception point) closes a real cross-agent identity bug that an earlier implementation had.
