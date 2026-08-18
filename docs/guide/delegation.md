# Agent-to-Agent Delegation

Any two agents registered on the same `Runtime` can see each other automatically — no extra wiring needed beyond `RegisterAgent`. Each agent gets a `delegate_to_<sibling-id>` tool per sibling, whose description includes that sibling's own tools and workflows, so the model can judge fitness rather than guess.

```go
rt.RegisterAgent(MyAgent())
rt.RegisterAgent(researchspecialist.Agent())
```

With both registered, `MyAgent` can decide mid-conversation to call `delegate_to_research_specialist` with a task description; that runs the specialist's own full loop synchronously and folds its answer back into the caller's own reply.

## Deliberate constraints

- **Delegation is synchronous** — the calling agent's loop blocks until the target finishes. There's no fire-and-forget agent-to-agent messaging today.
- **One level deep only** — a delegated-to agent can't itself delegate further. This prevents two agents' prompts both saying "if unsure, ask the other one" from creating an infinite back-and-forth. Enforced by a hard depth cap (`maxDelegationDepth`), not just by omission — a misconfigured cycle fails loudly rather than recursing forever.
- **A delegated agent never gets `send_message`**, even if it has its own messenger registered for unrelated direct use — its only valid answer channel during a delegated call is its own `end_loop`. It *does* still get platform-contributed tools (e.g. Slack's `add_reaction`) — see [Messaging](messaging.md).
- **A delegated agent resolves its own identities, not the delegator's** — if the target has its own `IdentifyAs` registered, its tools see that during the delegated call.
- **An agent with no `Messenger` is only reachable via delegation** — that's the whole point of `examples/researchspecialist`: it has no `AddMessenger` call, so nothing outside another agent's `delegate_to_research_specialist` call can reach it directly.
- **Approval-gated tools stay reachable regardless** — a tool with `RequiresApproval` lives in `a.Tools`, which is unconditionally included in every toolset (chat, workflow, delegated). See [Approval-Gated Tools](approval.md).

If you want a human to be able to reach a specific agent directly without going through another agent's judgment, the current answer is to give that agent its own separate `Messenger`/bot token — there's no router/addressing layer yet for multiple agents to share one channel.

## What actually happens

```go
func (a *Agent) delegateToolSpec(target *Agent) *tools.ToolSpec {
	description := fmt.Sprintf("Delegate a task to %s: %s\nIts capabilities:\n%s",
		target.Name, target.Description, target.capabilitiesSummary())
	return tools.NewToolBuilder("delegate_to_"+target.Id, description).
		Parameter("task", "string", "...", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			result, err := target.runDelegatedTask(ctx, input.Task)
			return fmt.Sprintf("%s finished (%s): %s", target.Name, result.Status, result.Content), nil
		}).Build()
}
```

`delegateToolSpecs()` builds one such tool per sibling visible through `a.directory.GetAgents()` (excluding self), computed fresh on every `RunLoop` call. The tool's description embeds the target's full capabilities summary, not just a one-line blurb, so the calling model can judge fitness for itself. Invoking it is synchronous — it blocks until the target's `runDelegatedTask` returns and folds the result straight into the delegator's own transcript. See [Architecture](../architecture/system.md) for the full internals.
