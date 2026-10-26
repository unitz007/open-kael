package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.AccountSettingsStore = (*Store)(nil)

// CountSessionsOf implements domain.AccountSettingsStore.
func (s *Store) CountSessionsOf(ctx context.Context, accountID string, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_sessions WHERE account_id = $1 AND expires_at > $2`, accountID, now).Scan(&n)
	return n, err
}

// DeleteOtherSessions implements domain.AccountSettingsStore.
func (s *Store) DeleteOtherSessions(ctx context.Context, accountID, keepToken string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE account_id = $1 AND token <> $2`, accountID, keepToken)
	return err
}

// SaveEmailChange implements domain.AccountSettingsStore.
func (s *Store) SaveEmailChange(ctx context.Context, accountID, newEmail, tokenHash string, expires time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM email_changes WHERE account_id = $1 OR expires_at < NOW()`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO email_changes (token_hash, account_id, new_email, expires_at) VALUES ($1, $2, $3, $4)`, tokenHash, accountID, newEmail, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PeekEmailChange implements domain.AccountSettingsStore.
func (s *Store) PeekEmailChange(ctx context.Context, tokenHash string, now time.Time) (string, string, error) {
	var accountID, email string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `SELECT account_id, new_email, expires_at FROM email_changes WHERE token_hash = $1`, tokenHash).Scan(&accountID, &email, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !now.Before(expires)) {
		return "", "", domain.ErrNotFound
	}
	return accountID, email, err
}

// TakeEmailChange implements domain.AccountSettingsStore: the link is removed in the same step it is read.
func (s *Store) TakeEmailChange(ctx context.Context, tokenHash string, now time.Time) (string, string, error) {
	var accountID, email string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM email_changes WHERE token_hash = $1 RETURNING account_id, new_email, expires_at`, tokenHash).Scan(&accountID, &email, &expires)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !now.Before(expires)) {
		return "", "", domain.ErrNotFound
	}
	return accountID, email, err
}
