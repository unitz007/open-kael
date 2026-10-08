package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SetMessengerChannelEmailLinkState(ctx context.Context, identityID, channelRef, state string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE messenger_channels SET email_link_state = $1
		WHERE identity_id = $2 AND channel_ref = $3
	`, state, identityID, channelRef)
	return err
}

func (s *Store) CreateEmailVerification(ctx context.Context, ev *domain.EmailVerification) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO email_verifications (token, user_id, identity_id, channel_ref, email, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (token) DO NOTHING
	`, ev.Token, ev.UserID, ev.IdentityID, ev.ChannelRef, ev.Email, ev.ExpiresAt)
	return err
}

func (s *Store) GetEmailVerificationByToken(ctx context.Context, token string) (*domain.EmailVerification, error) {
	ev := &domain.EmailVerification{}
	err := s.pool.QueryRow(ctx, `
		SELECT token, user_id, identity_id, channel_ref, email, expires_at
		FROM email_verifications WHERE token = $1
	`, token).Scan(&ev.Token, &ev.UserID, &ev.IdentityID, &ev.ChannelRef, &ev.Email, &ev.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return ev, err
}

func (s *Store) DeleteEmailVerification(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM email_verifications WHERE token = $1`, token)
	return err
}
