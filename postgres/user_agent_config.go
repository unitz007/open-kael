package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// GetUserAgentConfig returns the UserAgentConfig for (userID, agentID), or
// domain.ErrNotFound when the user hasn't set any personal instructions yet.
func (s *Store) GetUserAgentConfig(ctx context.Context, userID, agentID string) (*domain.UserAgentConfig, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT user_id, agent_id, instructions FROM user_agent_configs WHERE user_id = $1 AND agent_id = $2`,
		userID, agentID,
	)
	var cfg domain.UserAgentConfig
	if err := row.Scan(&cfg.UserID, &cfg.AgentID, &cfg.Instructions); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &cfg, nil
}

// SetUserAgentConfig upserts personal instructions for (cfg.UserID, cfg.AgentID).
func (s *Store) SetUserAgentConfig(ctx context.Context, cfg *domain.UserAgentConfig) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_agent_configs (user_id, agent_id, instructions)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, agent_id) DO UPDATE SET instructions = EXCLUDED.instructions
	`, cfg.UserID, cfg.AgentID, cfg.Instructions)
	return err
}

// MarkMessengerChannelOnboarded stamps onboarded_at = now() on the
// messenger_channel identified by (identityID, channelRef).
func (s *Store) MarkMessengerChannelOnboarded(ctx context.Context, identityID, channelRef string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messenger_channels SET onboarded_at = NOW() WHERE identity_id = $1 AND channel_ref = $2`,
		identityID, channelRef,
	)
	return err
}
