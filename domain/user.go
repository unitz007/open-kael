package domain

import "time"

// User is a platform end-user — someone who authenticates with the platform
// and interacts with Agents through it. Distinct from the creator who builds
// and configures Agents (the creator is the operator of the platform, not
// necessarily a User record in it).
type User struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	PasswordHash string `json:"-"` // never serialized; store-only field
}

// Session is an authenticated session for a User — an opaque token the client
// holds and presents on every request. Stored server-side so it can be
// revoked (logout) without a signing key round-trip.
type Session struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Session) Expired() bool { return time.Now().After(s.ExpiresAt) }
