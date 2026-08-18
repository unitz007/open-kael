# Known Issues

Severity-rated implementation issues. See [Known Limitations](../reference/known-limitations.md) for design-level gaps instead.

| Location | Issue | Severity |
|----------|-------|----------|
| `runtime/runtime.go` `Launch` | Agent goroutines are fire-and-forget — no `sync.WaitGroup` for graceful shutdown | Medium |
| Operational | Running two instances against the same bot token/connection silently steals or loses messages — logged (Telegram's `ok` check; Slack reconnects on error) but nothing prevents it | Medium |
| `human/human.go` | `Agent.human` is set nowhere and read nowhere — dead field | Low |
| `events/events.go` | Nothing in this module subscribes to the event bus itself | Low |
| Testing | The loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) have been manually/throwaway-tested during development but have no permanent test coverage | Low |

## Suggested Improvements

- [ ] Add `sync.WaitGroup` to `Runtime.Launch` for graceful shutdown
- [ ] Add tests for the tool-calling loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap)
- [ ] A router/addressing pattern for multiple agents to share one messenger channel (currently each agent needs its own bot/token to be directly reachable)
