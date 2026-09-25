package domain

import "time"

// ChannelLinkCode is a short-lived one-time token the bot generates when an
// unlinked user sends their first message. The flow:
//
//  1. Unlinked user messages the bot → runtime calls linkCodeGenerator(identityID, channelRef).
//  2. Runtime sends: "{frontendURL}/link?code=<Code>" to the user.
//  3. User clicks the link, signs up / logs in on the web app.
//  4. Web app calls POST /users/me/channels/redeem with the Code.
//  5. Server looks up the code → finds identity_id + channel_ref → creates
//     MessengerChannel linking the authenticated user to that chat.
type ChannelLinkCode struct {
	Code       string    `json:"code"`
	IdentityID string    `json:"identity_id"`
	ChannelRef string    `json:"channel_ref"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (c *ChannelLinkCode) Expired() bool { return time.Now().After(c.ExpiresAt) }
