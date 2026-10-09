package domain

import "time"

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
	ID          string     `json:"id"`
	IdentityID  string     `json:"identity_id"` // which bot/Identity this channel belongs to
	UserID      string     `json:"user_id"`
	ChannelRef  string     `json:"channel_ref"` // provider-specific address: Slack channel ID, Telegram ChatID, etc.
	SenderID    string     `json:"sender_id,omitempty"` // stable user identifier across channels (e.g. Slack user ID, Telegram user ID)
	OnboardingPromptedAt *time.Time `json:"onboarding_prompted_at,omitempty"` // nil until the onboarding prompt has been sent
	OnboardedAt          *time.Time `json:"onboarded_at,omitempty"`           // nil until the user completes the onboarding intro
	// EmailLinkState tracks progress through the optional email-linking flow.
	// Values: "" (not started), "awaiting" (prompt sent, waiting for email),
	// "sent" (verification email sent), "skipped" (user opted out).
	EmailLinkState string `json:"email_link_state,omitempty"`
}

// UserChannel is a deprecated alias kept during migration. Use MessengerChannel.
//
// Deprecated: remove once all callers are updated.
type UserChannel = MessengerChannel
