package domain

import (
	"context"
	"time"
)

// MinPasswordLength is the shortest password a login may have.
const MinPasswordLength = 8

// PasswordResetLife is how long a link to choose a new password works.
const PasswordResetLife = time.Hour

// PasswordResetStore keeps the one-time links people use to choose a new password: only a hash of each link is kept,
// one link per login at a time. A store that is not one cannot reset passwords.
type PasswordResetStore interface {
	// SavePasswordReset keeps a new link for a login, replacing any earlier one.
	SavePasswordReset(ctx context.Context, accountID, tokenHash string, expires time.Time) error
	// TakePasswordReset uses a link up: it returns the login it is for if it exists and has not run out, and it can
	// never be used again either way. ErrNotFound otherwise.
	TakePasswordReset(ctx context.Context, tokenHash string, now time.Time) (accountID string, err error)
	// DeleteSessionsOf signs a login out everywhere.
	DeleteSessionsOf(ctx context.Context, accountID string) error
}

// EmailChangeLife is how long a link to move a login to a new email works.
const EmailChangeLife = 24 * time.Hour

// AccountSettingsStore is what lets a signed-in creator manage their own login: where they are signed in, and moving it
// to a new email (by a one-time link to the new address; only a hash of each link is kept, one per login at a time).
type AccountSettingsStore interface {
	// CountSessionsOf is how many places a login is signed in on now.
	CountSessionsOf(ctx context.Context, accountID string, now time.Time) (int, error)
	// DeleteOtherSessions signs a login out everywhere but the session it keeps.
	DeleteOtherSessions(ctx context.Context, accountID, keepToken string) error
	// SaveEmailChange keeps a new link for a login, replacing any earlier one.
	SaveEmailChange(ctx context.Context, accountID, newEmail, tokenHash string, expires time.Time) error
	// PeekEmailChange says which login and new email a link is for, without using it up. ErrNotFound if it doesn't exist
	// or has run out.
	PeekEmailChange(ctx context.Context, tokenHash string, now time.Time) (accountID, newEmail string, err error)
	// TakeEmailChange uses a link up, as PeekEmailChange says; it can never be used again either way.
	TakeEmailChange(ctx context.Context, tokenHash string, now time.Time) (accountID, newEmail string, err error)
}
