package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveMessengerChannel(ctx context.Context, ch *domain.MessengerChannel) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (identity_id, channel_ref) DO UPDATE SET user_id = EXCLUDED.user_id
	`, ch.ID, ch.IdentityID, ch.UserID, ch.ChannelRef)
	return err
}

func (s *Store) GetMessengerChannelByIdentityAndRef(ctx context.Context, identityID, channelRef string) (*domain.MessengerChannel, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, channel_ref FROM messenger_channels WHERE identity_id = $1 AND channel_ref = $2`,
		identityID, channelRef,
	)
	ch, err := scanMessengerChannel(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return ch, err
}

func (s *Store) ListMessengerChannelsByUser(ctx context.Context, userID string) ([]*domain.MessengerChannel, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, identity_id, user_id, channel_ref FROM messenger_channels WHERE user_id = $1`,
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
	if err := row.Scan(&ch.ID, &ch.IdentityID, &ch.UserID, &ch.ChannelRef); err != nil {
		return nil, err
	}
	return &ch, nil
}
