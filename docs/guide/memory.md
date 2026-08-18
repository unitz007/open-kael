# Memory

```go
type Memory interface {
	History(ctx context.Context, id string) []llm.Message
	Append(ctx context.Context, id string, messages ...llm.Message)
}
```

No implementation ships in this module — same reasoning as `identity`/`webhook`/`rag`. `agent.NewAgent` defaults every agent to an internal, unexported, bare in-memory implementation — just enough to make an agent usable without calling `SetMemory` first, process-local, lost on restart, no persistence.

For anything that needs to survive a restart, `examples/starter` has two copyable references:

- **`InMemoryHistory`** — process-local, explicit (same shape as the internal default, but yours to hold a reference to and inspect).
- **`FileHistory`** — JSON-file-backed, atomic write-then-rename (a crash mid-write can't leave a half-written file).

Copy whichever fits, or bring your own database-backed implementation, and wire it in:

```go
a.SetMemory(myMemory) // call before Start / the first RunLoop — a change mid-conversation
                       // would silently orphan whatever was already recorded in the old store
```

`id` is an arbitrary string chosen by the caller — nothing in this interface assumes it's a conversation, a user, or anything else.

## How memory gets partitioned

**This is a configurable strategy, not a fixed rule.** `RunLoop`/`runWorkflow`/`runDelegatedTask` all derive their memory key via `Agent.SetMemoryKeyFunc(fn messaging.MemoryKeyFunc)`:

| Preset | Behavior |
|--------|----------|
| `messaging.KeyByAgent()` (default if unset) | One shared thread for the whole agent, regardless of platform/conversation/thread — an agent reachable on both Telegram and Slack remembers the same conversation across both, at the cost of nothing distinguishing *who* is messaging. |
| `messaging.KeyByConversation()` | Partitions by `ConversationRef` — each chat/channel gets its own thread. |
| `messaging.KeyByThread()` | Partitions by Slack thread specifically. |
| `messaging.KeyByWorkflow(fallback)` | Gives each workflow its own ongoing bucket across every run — including a reply that traces back to it via a Messenger-specific tagging mechanism (Slack: invisible message metadata read back via `conversations.replies`; see `messaging.WithWorkflowID`). Falls back to another `MemoryKeyFunc` for non-workflow calls. |

Compose these, or write your own `MemoryKeyFunc` — e.g. per-user isolation, which nothing ships today (no user-id field exists in the inbound pipeline), is exactly the kind of thing a custom strategy is for.
