package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// UserTimezone returns the person's IANA time zone name, or "" if they haven't said.
func (s *Store) UserTimezone(ctx context.Context, userID string) (string, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT timezone FROM users WHERE id = $1`, userID).Scan(&tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrNotFound
		}
		return "", err
	}
	return tz, nil
}

// SetUserTimezone records the person's IANA time zone name. The caller validates it.
func (s *Store) SetUserTimezone(ctx context.Context, userID, timezone string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET timezone = $2 WHERE id = $1`, userID, timezone)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetUserName records what the person wants to be called.
func (s *Store) SetUserName(ctx context.Context, userID, firstName, lastName string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET first_name = $2, last_name = $3 WHERE id = $1`, userID, firstName, lastName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
