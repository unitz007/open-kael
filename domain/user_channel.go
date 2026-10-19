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
	// EmailLinkStateAt is when EmailLinkState was last set.
	EmailLinkStateAt *time.Time `json:"email_link_state_at,omitempty"`
}

// EmailLinkAwaitingFor is how long a channel waits for the person to reply with their email address
// after tapping "Link Account". After that the next message is just a message again: without this,
// someone who tapped the button and wandered off would have their next ordinary message treated as
// an email address, and be asked for one again and again.
const EmailLinkAwaitingFor = 15 * time.Minute

// EffectiveEmailLinkState is EmailLinkState as it stands at now: a channel that has been "awaiting" an
// email for longer than EmailLinkAwaitingFor (or from before the time was kept) is no longer waiting.
func (c *MessengerChannel) EffectiveEmailLinkState(now time.Time) string {
	if c.EmailLinkState == "awaiting" && (c.EmailLinkStateAt == nil || now.Sub(*c.EmailLinkStateAt) > EmailLinkAwaitingFor) {
		return ""
	}
	return c.EmailLinkState
}

// UserChannel is a deprecated alias kept during migration. Use MessengerChannel.
//
// Deprecated: remove once all callers are updated.
type UserChannel = MessengerChannel

// sharedSenderServices are the messengers whose sender id names the same human on every bot of that
// service: one Telegram user id is that person whichever Telegram bot they write to. A Slack user id
// is only meaningful inside one workspace, so Slack is not here.
var sharedSenderServices = []string{"telegram", "discord", "whatsapp"}

// SenderSharedAcrossBots reports whether a messenger service's sender ids are the same human across
// all of its bots (see sharedSenderServices).
func SenderSharedAcrossBots(service string) bool {
	for _, s := range sharedSenderServices {
		if s == service {
			return true
		}
	}
	return false
}

// SharedSenderServices lists the services for which SenderSharedAcrossBots is true.
func SharedSenderServices() []string { return append([]string(nil), sharedSenderServices...) }

// DuplicateMember is one person in a DuplicatePeople group, with what is needed to choose who stays.
type DuplicateMember struct {
	PersonID      string
	Messages      int  // how much has been said to agents
	Channels      int  // how many chats they write from
	VerifiedEmail bool // they have a login with a verified email
}

// DuplicatePeople is a group of persons who are one human, found because they write from the same
// messenger account on different bots. Survivor is the one the others should be merged into.
type DuplicatePeople struct {
	Survivor string
	Members  []DuplicateMember // includes the survivor
	Via      []string          // the services that connect them, such as "telegram"
}
