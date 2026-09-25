package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveUser(ctx context.Context, u *domain.User) error {
	// Store empty email as NULL so the UNIQUE constraint allows multiple
	// messenger-provisioned users that haven't set an email yet.
	var email *string
	if u.Email != "" {
		email = &u.Email
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email, password_hash = EXCLUDED.password_hash
	`, u.ID, email, u.PasswordHash)
	return err
}

func (s *Store) GetUser(ctx context.Context, id string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, email, password_hash FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT id, email, password_hash FROM users WHERE email = $1`, email)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return u, err
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	var email *string
	if err := row.Scan(&u.ID, &email, &u.PasswordHash); err != nil {
		return nil, err
	}
	if email != nil {
		u.Email = *email
	}
	return &u, nil
}

func (s *Store) SaveSession(ctx context.Context, sess *domain.Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_sessions (token, user_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (token) DO UPDATE SET expires_at = EXCLUDED.expires_at
	`, sess.Token, sess.UserID, sess.ExpiresAt)
	return err
}

func (s *Store) GetSession(ctx context.Context, token string) (*domain.Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT token, user_id, expires_at FROM user_sessions WHERE token = $1`, token)
	var sess domain.Session
	if err := row.Scan(&sess.Token, &sess.UserID, &sess.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	return &sess, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token = $1`, token)
	return err
}
