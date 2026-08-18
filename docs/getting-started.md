# Getting Started

## Prerequisites

- Go `1.26+`
- An OpenAI-compatible chat-completions endpoint (e.g. OpenRouter) — the only hard dependency; everything else (a messenger, an identity, a specific tool's own API) is opt-in per agent.

## Install

```bash
go get github.com/unitz007/kael
```

Or, working from a clone of this repo:

```bash
go mod tidy
```

There's no config file — every piece of this module that needs configuration reads it from environment variables at the point it's actually used, not centrally.

## Running the example

```bash
cd examples/basic
export LLM_API_KEY="your-key"
export LLM_BASE_URL="https://openrouter.ai"
go run .
```

This registers an assistant agent and a delegation-only research specialist ([`examples/researchspecialist/agent.go`](https://github.com/unitz007/open-kael/blob/main/examples/researchspecialist/agent.go)) on a `Runtime` and launches it. Neither agent has a `Messenger` attached in the example, so nothing is reachable from outside the process yet — see [Messaging](guide/messaging.md) to wire one up.

`go build ./...` to just compile. `Ctrl+C` to stop.

!!! warning "Run only one instance per bot token"
    Telegram allows exactly one active `getUpdates` long-poll connection per bot token, and Slack's Socket Mode is the same — one active connection per token. A second instance — including one started from an IDE's own run button while a terminal instance is already up — doesn't error at startup; it silently competes for the same connection, and one of them stops receiving messages. See [Troubleshooting](reference/troubleshooting.md).

## Next steps

For a single agent showing every piece together — a custom `Identity`, a cron-triggered workflow, a webhook-triggered workflow, and a `Messenger` — see [`examples/starter`](https://github.com/unitz007/open-kael/tree/main/examples/starter). It's meant to be copied wholesale as the starting point for a real project.

Then work through the [Guide](guide/agents.md) for each piece: agents, tools, workflows, delegation, messaging, identity, approval-gated tools, and memory.
