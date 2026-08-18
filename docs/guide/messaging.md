# Messenger Interface

```go
type Messenger interface {
	Platform() string
	Send(ctx context.Context, conv ConversationRef, text string) error
	Listen(ctx context.Context, onMessage func(InboundMessage)) error
	DefaultConversation() ConversationRef
}
```

This module ships **no implementations** — `examples/messenger` is a working Slack/Telegram reference to copy into your own project and edit; wire up additional ones (a CLI, Discord, whatever) against the same interface and pass them to `AddMessenger`. An agent only gets `send_message` in its toolset once something's actually been registered — no messenger, no tool, rather than a tool that's guaranteed to fail the moment it's called.

## Optional extensions

A `Messenger` can additionally implement either of these — neither is required, both are checked structurally (type assertion), and `Agent.baseTools`/`callTool` pick them up automatically with no other wiring:

```go
type ToolProvider interface {
	Tools() []*tools.ToolSpec // platform-specific extras, e.g. Slack's add_reaction/search_emoji
}

type ApprovalMessenger interface {
	RequestApproval(ctx context.Context, conv ConversationRef, text string) (approved bool, err error)
}
```

`ToolProvider` is for capability that has no cross-platform equivalent (a reaction emoji doesn't mean anything on Telegram) — see `examples/messenger`'s `SlackBot`, which implements it for `add_reaction`/`search_emoji`. `ApprovalMessenger` is what makes a `RequiresApproval` tool actually work for that messenger — see [Approval-Gated Tools](approval.md). A messenger implementing neither still works as an ordinary `Messenger`; it just can't host an approval-gated tool for that agent (the call fails with a clear error rather than silently skipping the gate).

## Reference implementations (`examples/messenger`)

**Telegram** (`NewTelegramBot()`, reads `TELEGRAM_TOKEN`/`TELEGRAM_CHAT_ID`):

- Sends messages via the Telegram Bot API, converting markdown to Telegram-compatible HTML (bold, italic, code, links, ordered/unordered lists → bullets, thematic breaks → "———" since Telegram's HTML mode has no `<hr>`).
- Long-polls `getUpdates` for inbound text messages, logging Telegram's own error/description whenever a poll fails rather than silently treating a failure as "no new messages."
- Implements neither `ToolProvider` nor `ApprovalMessenger`.

**Slack** (`NewSlackBot()`, reads `SLACK_APP_TOKEN`/`SLACK_BOT_TOKEN`):

- Sends via `chat.postMessage`.
- Listens over Socket Mode (`apps.connections.open` + a websocket connection), acking each envelope and reconnecting automatically on disconnect.
- Implements both `ToolProvider` and `ApprovalMessenger`.

## Creating a custom Messenger

```go
package mymessaging

import (
	"context"

	"github.com/unitz007/kael/messaging"
)

type DiscordBot struct {
	Token string
}

func (d *DiscordBot) Platform() string { return "discord" }

func (d *DiscordBot) Send(ctx context.Context, conv messaging.ConversationRef, text string) error {
	// POST to the Discord API using conv.ChatID as the channel target
	return nil
}

func (d *DiscordBot) Listen(ctx context.Context, onMessage func(messaging.InboundMessage)) error {
	// gateway connection or webhook receiver; call onMessage for each inbound
	// message, return when ctx.Done()
	return nil
}

func (d *DiscordBot) DefaultConversation() messaging.ConversationRef {
	return messaging.ConversationRef{Platform: "discord", ChatID: "some-default-channel"}
}
```

Register it the same way as the reference implementations: `agent.AddMessenger(&DiscordBot{...})`. `ConversationRef{Platform, ChatID}` is how a reply gets routed back to the right place; `messaging.WithConversation`/`ConversationFromContext` thread the active one through `ctx` so a tool handler built with no knowledge of any specific conversation (or even of its own agent, in a workflow's case) can still find out where to reply.

!!! warning "One connection, one owner"
    Only one process should hold a given bot token's connection at a time — Telegram allows a single active `getUpdates` long-poll per token, and a duplicate process just steals or drops messages between the two silently. See [Troubleshooting](../reference/troubleshooting.md).
