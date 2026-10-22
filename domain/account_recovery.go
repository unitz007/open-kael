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
