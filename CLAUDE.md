# kael-platform

The reusable agent framework behind Kael — `agent`, `workflow`, `messaging`, `tools`, `triggers`, `webhook`, `memory`, `identity`, `runtime` packages. Module `github.com/unitz007/kael`, published as `open-kael` on GitHub. Consumed by the private `Kael` repo (a sibling directory) via a local `replace` directive in Kael's `go.mod`, and by `examples/` in this repo.

## Working rules

**Never implement, edit, build, commit, or push without an explicit go-ahead in the current message.** Diagnose and explain by default; propose a change and stop. This framework is consumed by a real deployed app (Kael) — an unrequested change here doesn't just affect this repo, it affects Kael's next build.

## Design conventions worth preserving

**`Agent.IdentityPrompt` vs `Workflow.SystemPrompt` are different scopes — don't blur them.** `IdentityPrompt` is prepended to *every* conversation the agent has, across every workflow and every ad-hoc reply (see `Agent.RunLoop`/`baseTools()`). `Workflow.SystemPrompt` only loads for that one workflow's run, and does **not** inherit `IdentityPrompt` — a workflow needing something from the agent's identity has to restate it. When adding a new capability, think about which scope the instruction actually belongs in before defaulting to `IdentityPrompt`; it's the one place a consuming app (Kael) is most likely to pile up unrelated, narrow instructions over time because it's always in scope.

**`messaging.ToolProvider` is the extension point for platform-specific tools with no cross-platform equivalent** (see `messaging/messaging.go`) — e.g. Slack message reactions. A `Messenger` implementation opts in by implementing `Tools() []*tools.ToolSpec`; `Agent.baseTools()` picks these up automatically for any agent with that messenger registered. This is deliberately the *only* sanctioned way platform-specific behavior enters an agent's toolset — don't special-case a platform inside `agent`/`workflow` core logic; add a `ToolProvider` method on the messenger instead, and gate the tool itself with `.Platform("<name>")` (see `tools.ToolSpec`).

**Tool descriptions carry tool-usage guidance; they're read by the model exactly when it matters** (deciding whether/how to call that tool) and disappear automatically when the tool isn't registered. Prefer putting "when to use this / how to sequence it with other tools" guidance in the `tools.NewToolBuilder(...)` description string over pushing it into a consuming app's system prompt — a consumer duplicating tool-usage guidance into its own prompt text is a sign the tool's own description is incomplete.

**Context-carried values (`messaging.WithThreadID`, `WithConversation`, `WithMessageID`, `WithWorkflowID`, etc.) are the platform-agnostic plumbing** — a `Messenger`'s own `Send`/`SendGetID` implementation reads these off `ctx` rather than every caller threading a thread/conversation param through explicitly. Keep it that way: a tool handler that needs "which thread is this" should read `ThreadIDFromContext`, not assume a specific messenger's own concept of a thread.

## Downstream build

Kael's `go.mod` has `replace github.com/unitz007/kael => ../kael-platform` for local development — changes here are visible to a local Kael build immediately. Production deploys build from a real Docker context that still needs the sibling directory present (see Kael's own `CLAUDE.md`), and any change meant to ship needs `go build ./... && go vet ./...` clean here first, then committing and pushing to `open-kael` before Kael's next deploy will pick it up.
