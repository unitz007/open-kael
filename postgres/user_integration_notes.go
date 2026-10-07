package postgres

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) ListUserIntegrationNotesByUser(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT integration_id, notes FROM user_integration_notes WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var n domain.UserIntegrationNotes
		if err := rows.Scan(&n.IntegrationID, &n.Notes); err != nil {
			return nil, err
		}
		out[n.IntegrationID] = n.Notes
	}
	return out, rows.Err()
}

func (s *Store) SetUserIntegrationNotes(ctx context.Context, userID, integrationID, notes string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_integration_notes (user_id, integration_id, notes)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, integration_id) DO UPDATE SET notes = EXCLUDED.notes
	`, userID, integrationID, notes)
	return err
}
