package domain

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
		},
		Required: []string{"recipient", "text"},
	}
}

// SendMessageOutputSchema is the canonical output contract for
// ActionSendMessage, identical across every provider — regardless of what
// a provider natively calls its own message identifier (Slack's ts,
// Telegram's numeric message ID), it comes back as message_id.
func SendMessageOutputSchema() Schema {
	return Schema{
		Type: SchemaTypeObject,
		Properties: map[string]Schema{
			"message_id": {
				Type:        SchemaTypeString,
				Description: "Provider-specific identifier of the sent message.",
			},
		},
		Required: []string{"message_id"},
	}
}
