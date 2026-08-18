# Known Limitations

- **No graceful shutdown** — agent goroutines in `Runtime.Launch` are fire-and-forget, no `sync.WaitGroup`.
- **Double JSON unmarshal** — every tool handler receives a JSON string wrapper around its actual arguments (see [Tools](../guide/tools.md)).
- **`EventTriggerType` unimplemented** — declared but has no handling in `Agent.Start`; `CronTriggerType` and `WebhookTriggerType` both run for real.
- **No shared-channel routing** — each agent needs its own `Messenger`/bot token to be directly reachable by a human; there's no way yet for multiple agents to field messages from one shared channel and have a human pick which one to address.
- **No built-in per-user memory partitioning preset** — see [Memory](../guide/memory.md); write a custom `MemoryKeyFunc` if you need it, not a fit for a multi-tenant deployment out of the box otherwise.
- **No test coverage** for the loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) yet — manually verified during development only.
- **A delegated agent's use of `send_message`/reaction tools depends on the model choosing to** — the tools are offered where applicable, but nothing forces a delegate to call them; in practice, models observed during development reliably skip them on a simple, single-answer task and just rely on `end_loop`'s relay instead. Not a bug, just worth knowing if you're depending on it for something visible to the user.

See [Known Issues](../architecture/known-issues.md) for implementation-level issues (severity-rated) rather than design-level limitations.
