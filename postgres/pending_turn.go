package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// SavePendingTurn keeps what a person asked for until they connect what it needs.
// A newer request for the same connection replaces the older one.
func (s *Store) SavePendingTurn(ctx context.Context, t *domain.PendingTurn) error {
	conv, err := json.Marshal(t.Conv)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO pending_turns (user_id, identity_id, agent_id, conv, message, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (user_id, identity_id) DO UPDATE SET
			agent_id   = EXCLUDED.agent_id,
			conv       = EXCLUDED.conv,
			message    = EXCLUDED.message,
			created_at = NOW()
	`, t.UserID, t.IdentityID, t.AgentID, conv, t.Message)
	return err
}

// TakePendingTurn returns and removes the held request for (user, connection). A
// request older than maxAge is discarded and reported as not found: an instruction
// from long ago must never be carried out as if it were fresh.
func (s *Store) TakePendingTurn(ctx context.Context, userID, identityID string, maxAge time.Duration) (*domain.PendingTurn, error) {
	var (
		t    domain.PendingTurn
		conv []byte
	)
	err := s.pool.QueryRow(ctx, `
		DELETE FROM pending_turns WHERE user_id = $1 AND identity_id = $2
		RETURNING user_id, identity_id, agent_id, conv, message, created_at
	`, userID, identityID).Scan(&t.UserID, &t.IdentityID, &t.AgentID, &conv, &t.Message, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if maxAge > 0 && time.Since(t.CreatedAt) > maxAge {
		return nil, domain.ErrNotFound
	}
	if err := json.Unmarshal(conv, &t.Conv); err != nil {
		return nil, err
	}
	return &t, nil
}
