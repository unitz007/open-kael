package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) GetUserProfile(ctx context.Context, userID string) (*domain.UserProfile, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT user_id, notes FROM user_profiles WHERE user_id = $1`,
		userID,
	)
	var p domain.UserProfile
	if err := row.Scan(&p.UserID, &p.Notes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &p, nil
}

func (s *Store) SetUserProfile(ctx context.Context, userID, notes string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_profiles (user_id, notes)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET notes = EXCLUDED.notes
	`, userID, notes)
	return err
}
