package domain

import "context"

// TurnNotifier is optionally implemented by messengers to show an in-progress
// indicator while a turn is being processed (e.g. Telegram's "typing" action,
// Slack's typing indicator). The runtime discovers it via type assertion and
// calls NotifyThinking before entering the agent loop; the returned stop func
// is called once the reply is ready to be sent.
//
// identity and connectionRef follow the same multi-tenant credential pattern
// as Executor.Execute — one Executor instance serves every connected bot, so
// which credentials to use can't be baked in at construction time.
type TurnNotifier interface {
	// NotifyThinking starts the in-progress indicator for chatID.
	// The returned stop func must be called exactly once (safe to call multiple
	// times — idempotent). It is called by the runtime before reply delivery.
	NotifyThinking(ctx context.Context, identity *Identity, connectionRef, chatID string) (stop func(), err error)
}

// StreamingMessenger is optionally implemented by messengers to deliver a
// reply progressively — sending a placeholder and editing it as content
// accumulates — rather than one atomic send. The runtime feeds content over
// chunks (word groups or raw LLM token deltas); close the channel to signal
// completion. StreamReply blocks until the stream is fully consumed and the
// final message is delivered, then returns the sent message's IDs.
//
// The message_id and thread_id in the return match ActionSendMessage's
// output contract so callers can reply within the same thread later.
// Implementations must tolerate a closed or cancelled channel gracefully.
type StreamingMessenger interface {
	StreamReply(ctx context.Context, identity *Identity, connectionRef, chatID, threadID string, chunks <-chan string) (messageID, replyThreadID string, err error)
}

// ActionSendMessage is the canonical dispatch key a messenger-capable
// provider's Executor responds to. Unlike a provider-specific Action (e.g.
// executors/slack's "slack.post_message"), this one is shared: every
// messenger provider (Slack, Telegram, ...) implements the same Action
// against the same InputSchema/OutputSchema, so a Skill — or an Agent's own
// default tools — can send a message without knowing which provider is
// actually behind it. Provider-specific power (Slack's add_reaction, or
// anything a shared contract can't express) stays on that provider's own,
// separately-Actioned tools; this is deliberately the lowest common shape,
// not a replacement for them.
const ActionSendMessage = "messenger.send_message"

// SendMessageInputSchema is the canonical input contract for
// ActionSendMessage, identical across every provider. recipient is
// whatever identifier that provider uses to address a destination — a
// channel, a group, or a specific person. There is no separate DM shape:
// Slack and Telegram both already treat a person as just another
// addressable destination (a user ID in place of a channel ID), so a
// direct message is simply a recipient value, not a different field.
//
// thread_id is optional and, like recipient, opaque to the caller — pass
// back a thread_id a prior ActionSendMessage call returned to reply within
// that same thread/conversation (Slack's thread_ts, Telegram's
// reply_to_message_id); omit it to start a new one.
//
// A function, not a package-level var, so every caller gets its own Schema
// value — Schema's Properties/Required are mutable, and a shared var would
// let one provider's tool constructor accidentally mutate what another
// provider's tool sees.
func SendMessageInputSchema() Schema {
	return Schema{
		Type: SchemaTypeObject,
		Properties: map[string]Schema{
			"recipient": {
				Type:        SchemaTypeString,
				Description: "Who or where to send the message — a channel, group, or user ID/handle, in whatever form this provider addresses destinations.",
			},
			"text": {
				Type:        SchemaTypeString,
				Description: "Message text.",
			},
			"thread_id": {
				Type:        SchemaTypeString,
				Description: "Opaque identifier of an existing thread/conversation to reply within, as returned by a prior send on this provider. Omit to start a new one.",
			},
		},
		Required: []string{"recipient", "text"},
	}
}

// SendMessageOutputSchema is the canonical output contract for
// ActionSendMessage, identical across every provider — regardless of what
// a provider natively calls its own message identifier (Slack's ts,
// Telegram's numeric message ID), it comes back as message_id.
//
// thread_id is always populated: the input thread_id if one was given, or
// the identifier of the thread this message just opened otherwise. Pass it
// back on a later call to reply within the same thread.
func SendMessageOutputSchema() Schema {
	return Schema{
		Type: SchemaTypeObject,
		Properties: map[string]Schema{
			"message_id": {
				Type:        SchemaTypeString,
				Description: "Provider-specific identifier of the sent message.",
			},
			"thread_id": {
				Type:        SchemaTypeString,
				Description: "Opaque identifier of the thread this message belongs to — pass this back as thread_id to reply within it.",
			},
		},
		Required: []string{"message_id", "thread_id"},
	}
}
