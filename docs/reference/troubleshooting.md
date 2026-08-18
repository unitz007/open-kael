# Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| App exits immediately at startup | Missing `LLM_API_KEY` or `LLM_BASE_URL` | Set both environment variables |
| No reply to a message, no errors logged | A second process is using the same bot token/connection | Check for a duplicate process, kill it, restart one instance |
| `telegram getUpdates failed (code 409): Conflict...` | Same as above, for Telegram — surfaced explicitly in logs | Same fix |
| `⏱️...hit the N-iteration cap without calling end_loop` | The model never produced a valid final answer within `MaxIterations` (6 by default) | Usually a sign the request needs breaking down, or that a tool is failing repeatedly — check for `Tool execution error` lines just before it |
| `⚠️...returned plain text instead of a tool call` | The model responded without calling any tool — the loop nudges it to retry rather than accepting unverified content; not by itself an error, just a retry cost | If it happens repeatedly for the same kind of request, tighten that agent/workflow's prompt. If it persists even after a retry, check the logged `finish_reason`/`reasoning` — a reasoning-capable model may need `enableReasoning: false` (see `examples/llm/openai.NewClient`) |
| `⚠️...response contained N tool calls in one turn` | The model's response degenerated into repeating the same tool call many times (a decoding glitch) — the whole batch is discarded automatically | Not actionable beyond noting which request triggered it; retry usually succeeds |
| `⚠️...tried to repeat ... — blocked` | The model tried to call the same tool with the same arguments twice after it already succeeded | Expected behavior, not a bug — if the model keeps needing this, its prompt may be unclear about when to call `end_loop` |
| "agent ... already exists in the registry" log | Duplicate `RegisterAgent` call with the same `Id` | Agent IDs must be unique |
| `send_message: could not resolve a target` / `no messenger registered` | The agent has no `Messenger` registered, or the active conversation's platform doesn't match any registered messenger | Call `AddMessenger` for that platform, or don't expose `send_message` to that agent at all |
| `<tool> requires approval, but <platform> doesn't support interactive approval` | The tool has `RequiresApproval: true` but the resolved messenger doesn't implement `messaging.ApprovalMessenger` | Implement `ApprovalMessenger` on that messenger, or don't gate the tool for an agent using it |
| A panic's stack trace appears in logs but the process keeps running | Expected — recovered by `recoverFromPanic` at the relevant goroutine entry point, not a crash | Fix the root cause the stack trace points to; the recovery is a containment measure, not a fix |

## Debug tips

- Startup: `🎯kael started successfully with N agent(s).`
- Per-agent startup: `🤖{AgentName} started successfully`
- Message handling: `💬{AgentName}: handling message from ...` → `💬{AgentName}: finished handling message ... status=... iterations=N`
- Outbound reply: `📤{AgentName}: routing send_message to {platform}/{chatID}`
- Workflow trigger (cron or webhook): `🧨{WorkflowName} for {AgentName} Triggered` — runs for real, see [Workflows & Triggers](../guide/workflows.md)
- Delegation: `🤝{AgentName}: delegating to {Target}: "..."` → `🤝{AgentName}: {Target} finished (status): ...`
- Panic recovery: `⚠️{AgentName}: recovered from panic in {context}: ...` followed by a stack trace
