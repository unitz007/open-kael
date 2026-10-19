package domain

import "time"

// The platform knows two different things that used to be one "user".
//
// A Person is a human who talks to agents. Everything about them as someone being talked to hangs on
// it: the messenger accounts they write from (see MessengerChannel), what the agents have learned
// about them, their app connections, tasks and settings. A person is created the first time someone
// writes to a bot, and needs no login.
//
// An Account is a login: an email and, optionally, a password. It is what signs in to the web app
// and what owns what a creator builds (agents, connections, bots: their CreatedBy). An account may
// be linked to a person (that is how a creator also chats with agents, and how someone who writes
// from Telegram signs in to the web), and several accounts may point at the same person.

// Person is a human who talks to agents.
type Person struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	Language  string `json:"language,omitempty"`
	Country   string `json:"country,omitempty"`
	City      string `json:"city,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	// MergedInto is set on a person who was found to be someone already known and folded into them
	// (see Store.MergePersons). The row stays so an old id still leads to the right person.
	MergedInto string `json:"merged_into,omitempty"`
}

// FullName is the name as the person gave it, first and last.
func (p *Person) FullName() string {
	switch {
	case p.FirstName == "":
		return p.LastName
	case p.LastName == "":
		return p.FirstName
	}
	return p.FirstName + " " + p.LastName
}

// Account is a login to the web app, and the owner of what a creator builds.
type Account struct {
	ID                string `json:"id"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PasswordHash      string `json:"-"` // never serialized; store-only field
	VerificationToken string `json:"-"` // never serialized; store-only field
	// PersonID is the person this login is for ("" when none yet). It is how the web app knows which
	// person's connections, approvals and settings a signed-in account is managing.
	PersonID  string    `json:"person_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is an authenticated session for an Account — an opaque token the client
// holds and presents on every request. Stored server-side so it can be
// revoked (logout) without a signing key round-trip.
type Session struct {
	Token     string    `json:"token"`
	AccountID string    `json:"account_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Session) Expired() bool { return time.Now().After(s.ExpiresAt) }
