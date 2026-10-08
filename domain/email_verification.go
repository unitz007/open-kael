package domain

import "time"

// EmailVerification is a short-lived record created when a user starts the
// email-linking flow. The token is emailed to the user as a verify link;
// on click the server marks the user's email verified and reassigns the
// messenger channel to the canonical user if the email already existed.
type EmailVerification struct {
	Token      string
	UserID     string
	IdentityID string
	ChannelRef string
	Email      string
	ExpiresAt  time.Time
}
