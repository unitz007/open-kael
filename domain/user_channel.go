package domain

// MessengerChannel maps a platform user to their address on a specific
// messaging Identity (Telegram bot, Slack bot, Discord bot, MS Teams bot).
// It is the user-side counterpart for messaging providers — the bridge between
// "Alice on the platform" and "Telegram chat ID 12345 on Bot A".
//
// Created automatically when a user first messages the bot (inbound message
// handler) or via the link-code redemption flow. Unlike AppAuthorization it
// carries no auth credential — the bot token lives on the Identity; the
// ChannelRef is only a routing address.
type MessengerChannel struct {
	ID         string `json:"id"`
	IdentityID string `json:"identity_id"` // which bot/Identity this channel belongs to
	UserID     string `json:"user_id"`
	ChannelRef string `json:"channel_ref"` // provider-specific address: Telegram ChatID, Slack UserID, Discord UserID, etc.
}

// UserChannel is a deprecated alias kept during migration. Use MessengerChannel.
//
// Deprecated: remove once all callers are updated.
type UserChannel = MessengerChannel
