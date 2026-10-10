package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetUserAppInstructions returns the per-app instructions for (userID,
// integrationID). Returns an empty string when no instructions are set.
func (s *Store) GetUserAppInstructions(ctx context.Context, userID, integrationID string) (string, error) {
	var instructions string
	err := s.pool.QueryRow(ctx,
		`SELECT instructions FROM user_app_instructions WHERE user_id = $1 AND integration_id = $2`,
		userID, integrationID,
	).Scan(&instructions)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return instructions, nil
}

// SetUserAppInstructions upserts per-app instructions for (userID,
// integrationID). Calling with an empty text removes any existing row.
func (s *Store) SetUserAppInstructions(ctx context.Context, userID, integrationID, text string) error {
	if text == "" {
		_, err := s.pool.Exec(ctx,
			`DELETE FROM user_app_instructions WHERE user_id = $1 AND integration_id = $2`,
			userID, integrationID,
		)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_app_instructions (user_id, integration_id, instructions, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (user_id, integration_id) DO UPDATE
		SET instructions = EXCLUDED.instructions, updated_at = NOW()
	`, userID, integrationID, text)
	return err
}
