package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

const accountCols = `id, email, password_hash, email_verified, verification_token, person_id, created_at`

func (s *Store) SaveAccount(ctx context.Context, a *domain.Account) error {
	// An empty email is stored as NULL so the UNIQUE constraint allows several logins without one.
	var email, person *string
	if e := domain.NormalizeEmail(a.Email); e != "" {
		email = &e
	}
	if a.PersonID != "" {
		person = &a.PersonID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO accounts (id, email, password_hash, email_verified, verification_token, person_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			email              = EXCLUDED.email,
			password_hash      = EXCLUDED.password_hash,
			email_verified     = EXCLUDED.email_verified,
			verification_token = EXCLUDED.verification_token,
			person_id          = EXCLUDED.person_id
	`, a.ID, email, a.PasswordHash, a.EmailVerified, a.VerificationToken, person)
	return err
}

func (s *Store) getAccountWhere(ctx context.Context, where string, arg any) (*domain.Account, error) {
	a, err := scanAccount(s.pool.QueryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

func (s *Store) GetAccount(ctx context.Context, id string) (*domain.Account, error) {
	return s.getAccountWhere(ctx, `id = $1`, id)
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (*domain.Account, error) {
	return s.getAccountWhere(ctx, `email = $1`, domain.NormalizeEmail(email))
}

func (s *Store) GetAccountByVerificationToken(ctx context.Context, token string) (*domain.Account, error) {
	return s.getAccountWhere(ctx, `verification_token = $1 AND verification_token <> ''`, token)
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	return err
}

func (s *Store) ListAccountsByPerson(ctx context.Context, personID string) ([]*domain.Account, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+accountCols+` FROM accounts WHERE person_id = $1 ORDER BY created_at, id`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAccount(row pgx.Row) (*domain.Account, error) {
	var a domain.Account
	var email, person *string
	if err := row.Scan(&a.ID, &email, &a.PasswordHash, &a.EmailVerified, &a.VerificationToken, &person, &a.CreatedAt); err != nil {
		return nil, err
	}
	if email != nil {
		a.Email = *email
	}
	if person != nil {
		a.PersonID = *person
	}
	return &a, nil
}

func (s *Store) SaveSession(ctx context.Context, sess *domain.Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_sessions (token, account_id, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (token) DO UPDATE SET expires_at = EXCLUDED.expires_at
	`, sess.Token, sess.AccountID, sess.ExpiresAt)
	return err
}

func (s *Store) GetSession(ctx context.Context, token string) (*domain.Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT token, account_id, expires_at FROM user_sessions WHERE token = $1`, token)
	var sess domain.Session
	if err := row.Scan(&sess.Token, &sess.AccountID, &sess.ExpiresAt); err != nil {
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
