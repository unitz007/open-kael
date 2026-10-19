package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveMessengerChannel(ctx context.Context, ch *domain.MessengerChannel) error {
	var inserted bool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref, sender_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (identity_id, channel_ref) DO UPDATE SET user_id = EXCLUDED.user_id, sender_id = EXCLUDED.sender_id
		RETURNING (xmax = 0)
	`, ch.ID, ch.IdentityID, ch.UserID, ch.ChannelRef, ch.SenderID).Scan(&inserted)
	if err == nil && inserted {
		// Someone new connected: the creator of every agent using this bot sees a count, not who.
		s.recordJoined(ctx, ch.IdentityID)
	}
	return err
}

func (s *Store) GetMessengerChannelByIdentityAndRef(ctx context.Context, identityID, channelRef string) (*domain.MessengerChannel, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, channel_ref, sender_id, onboarding_prompted_at, onboarded_at, email_link_state, email_link_state_at FROM messenger_channels WHERE identity_id = $1 AND channel_ref = $2`,
		identityID, channelRef,
	)
	ch, err := scanMessengerChannel(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return ch, err
}

func (s *Store) GetMessengerChannelByIdentityAndSender(ctx context.Context, identityID, senderID string) (*domain.MessengerChannel, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, channel_ref, sender_id, onboarding_prompted_at, onboarded_at, email_link_state, email_link_state_at FROM messenger_channels WHERE identity_id = $1 AND sender_id = $2 LIMIT 1`,
		identityID, senderID,
	)
	ch, err := scanMessengerChannel(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return ch, err
}

func (s *Store) MarkMessengerChannelOnboardingPrompted(ctx context.Context, identityID, channelRef string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messenger_channels SET onboarding_prompted_at = NOW() WHERE identity_id = $1 AND channel_ref = $2`,
		identityID, channelRef,
	)
	return err
}

func (s *Store) ListMessengerChannelsByUser(ctx context.Context, userID string) ([]*domain.MessengerChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, identity_id, user_id, channel_ref, sender_id, onboarding_prompted_at, onboarded_at, email_link_state, email_link_state_at FROM messenger_channels WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.MessengerChannel
	for rows.Next() {
		ch, err := scanMessengerChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMessengerChannel(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM messenger_channels WHERE id = $1`, id)
	return err
}

func scanMessengerChannel(row pgx.Row) (*domain.MessengerChannel, error) {
	var ch domain.MessengerChannel
	if err := row.Scan(&ch.ID, &ch.IdentityID, &ch.UserID, &ch.ChannelRef, &ch.SenderID, &ch.OnboardingPromptedAt, &ch.OnboardedAt, &ch.EmailLinkState, &ch.EmailLinkStateAt); err != nil {
		return nil, err
	}
	return &ch, nil
}

// SaveWelcomeMessage remembers which message carries the "Get started" button.
func (s *Store) SaveWelcomeMessage(ctx context.Context, identityID, channelRef, messageID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE messenger_channels SET welcome_message_id = $3 WHERE identity_id = $1 AND channel_ref = $2`,
		identityID, channelRef, messageID)
	return err
}

// TakeWelcomeMessage returns the remembered welcome message ID and forgets it, so the
// button is only ever taken off once. Empty when there is none.
func (s *Store) TakeWelcomeMessage(ctx context.Context, identityID, channelRef string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT id, welcome_message_id FROM messenger_channels
			WHERE identity_id = $1 AND channel_ref = $2 FOR UPDATE
		), cleared AS (
			UPDATE messenger_channels SET welcome_message_id = ''
			WHERE id IN (SELECT id FROM old)
		)
		SELECT welcome_message_id FROM old`, identityID, channelRef).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}
