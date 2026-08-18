# File Map

| File | Responsibility |
|------|----------------|
| `agent/agent.go` | `Agent` type, the loop, `RunLoop`/`runWorkflow`/`runDelegatedTask`, `callTool` (approval gating), workflows-as-tools, delegation, identity, panic recovery |
| `agent/inbox.go` | `MessageQueue` — buffered inbound message queue, panic-recovered per message |
| `runtime/runtime.go` | Agent registry, shared event bus, shared webhook mux, `Launch` |
| `messaging/messaging.go` | `Messenger`/`ToolProvider`/`ApprovalMessenger` interfaces, `ConversationRef`, context helpers — no implementations |
| `webhook/webhook.go` | `Source` interface (the webhook counterpart to `Messenger`), `VerifyHMACSHA256` — no implementations |
| `examples/messenger/telegram.go` | Telegram `Send`/`Listen` + markdown→HTML formatting (copyable reference, not imported) |
| `examples/messenger/slack.go`, `slack_tools.go` | Slack Socket Mode `Send`/`Listen`, `ApprovalMessenger`/`ToolProvider` implementations (copyable reference) |
| `identity/identity.go` | `Identity` interface, context helpers — no implementations |
| `memory/memory.go` | `Memory` interface only — no implementation |
| `examples/starter/memory.go`, `memory_file.go` | `InMemoryHistory`, `FileHistory` — copyable references, not imported |
| `workflow/workflow.go` | `Workflow` struct |
| `triggers/trigger.go` | `TriggerType`, `Trigger` |
| `tools/tool.go` | `ToolSpec`, `ToolSpecBuilder` |
| `llm/llm.go` | Provider-agnostic `LLM` interface, `Message`/`Response` |
| `examples/llm/openai/` | OpenAI-compatible chat-completions client |
| `events/events.go` | `EventBus` |
| `human/human.go` | Placeholder type, currently unused |
| `examples/researchspecialist/agent.go` | Delegation-only demo agent |
| `examples/basic/main.go` | Minimal runnable two-agent example |
| `examples/starter/` | Full single-agent reference: identity + memory + cron + webhook + messenger together |
