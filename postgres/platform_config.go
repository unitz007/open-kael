package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) GetPlatformConfig(ctx context.Context, key string) (string, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT value FROM platform_config WHERE key = $1`,
		key,
	)
	var value string
	if err := row.Scan(&value); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrNotFound
		}
		return "", err
	}
	return value, nil
}

func (s *Store) SetPlatformConfig(ctx context.Context, key, value string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO platform_config (key, value)
		VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, value)
	return err
}
