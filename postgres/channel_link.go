package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveChannelLinkCode(ctx context.Context, c *domain.ChannelLinkCode) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO channel_link_codes (code, identity_id, channel_ref, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO UPDATE SET expires_at = EXCLUDED.expires_at
	`, c.Code, c.IdentityID, c.ChannelRef, c.ExpiresAt)
	return err
}

func (s *Store) GetChannelLinkCode(ctx context.Context, code string) (*domain.ChannelLinkCode, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT code, identity_id, channel_ref, expires_at FROM channel_link_codes WHERE code = $1
	`, code)
	var c domain.ChannelLinkCode
	if err := row.Scan(&c.Code, &c.IdentityID, &c.ChannelRef, &c.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (s *Store) DeleteChannelLinkCode(ctx context.Context, code string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM channel_link_codes WHERE code = $1`, code)
	return err
}
