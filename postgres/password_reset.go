package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.PasswordResetStore = (*Store)(nil)

// SavePasswordReset implements domain.PasswordResetStore.
func (s *Store) SavePasswordReset(ctx context.Context, accountID, tokenHash string, expires time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM password_resets WHERE account_id = $1 OR expires_at < NOW()`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO password_resets (token_hash, account_id, expires_at) VALUES ($1, $2, $3)`, tokenHash, accountID, expires); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TakePasswordReset implements domain.PasswordResetStore: the link is removed in the same step it is read, so two
// uses at once cannot both succeed.
func (s *Store) TakePasswordReset(ctx context.Context, tokenHash string, now time.Time) (string, error) {
	var accountID string
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM password_resets WHERE token_hash = $1 RETURNING account_id, expires_at`, tokenHash).Scan(&accountID, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !now.Before(expires) {
		return "", domain.ErrNotFound
	}
	return accountID, nil
}

// DeleteSessionsOf implements domain.PasswordResetStore.
func (s *Store) DeleteSessionsOf(ctx context.Context, accountID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE account_id = $1`, accountID)
	return err
}
