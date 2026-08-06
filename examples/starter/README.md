# Starter

A single runnable agent showing every core piece together, meant to be copied wholesale as a starting point for a real project — nothing here is meant to stay imported as a dependency.

- **`identity.go`** — a minimal `identity.Identity` (`exampleIdentity`) plus a `whoami` tool that resolves it via `identity.FromContext`, the way any real tool would.
- **`memory.go`** — a minimal `memory.Memory` (`InMemoryHistory`), process-local and lost on restart.
- **`memory_file.go`** — a JSON-file-backed `memory.Memory` (`FileHistory`) that survives a restart — not wired into `main.go` by default, swap it in for `InMemoryHistory` to see the difference.
- **`webhook_source.go`** — a minimal `webhook.Source` (`exampleWebhookSource`), HMAC-verified via `webhook.VerifyHMACSHA256`.
- **`main.go`** — wires all of it onto one agent: the identity above, `InMemoryHistory` as memory, a cron-triggered workflow (`daily_report_wf`), a webhook-triggered workflow (`on_webhook_wf`), and a Slack messenger from [`examples/messenger`](../messenger).

## Run it

```bash
export LLM_API_KEY="your-key"
export LLM_BASE_URL="https://openrouter.ai"
go run .
```

That's the minimum. Everything else is optional and just leaves the corresponding piece non-functional until set:

| Variable | Enables |
|---|---|
| `SLACK_BOT_TOKEN` / `SLACK_APP_TOKEN` / `SLACK_CHANNEL_ID` | Slack messaging |
| `EXAMPLE_API_TOKEN` | the `whoami` tool actually returning a token |
| `EXAMPLE_WEBHOOK_SECRET` | `POST http://localhost:8080/webhooks/example` with a valid signature |

## Extending this

- Swap `messenger.NewSlackBot(...)` for `messenger.NewTelegramBot(...)` (see [`examples/messenger/telegram.go`](../messenger/telegram.go)) — or write your own against `messaging.Messenger`.
- Replace `exampleIdentity` with a real one — a GitHub App installation token, AWS STS, whatever your tools actually need to authenticate against.
- Swap `InMemoryHistory` for `FileHistory`, or write your own against `memory.Memory` for a real database.
- Replace `exampleWebhookSource` with a real sender's verification scheme (GitHub's `X-Hub-Signature-256`, Stripe's `Stripe-Signature`, ...).
- Add `AllowDelegation: true` to a workflow and register a second agent on the same `Runtime` to see delegation — or copy [`examples/basic`](../basic) for a dedicated delegation-only example.
