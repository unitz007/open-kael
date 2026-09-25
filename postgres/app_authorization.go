package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveAppAuthorization(ctx context.Context, a *domain.AppAuthorization) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO app_authorizations (id, identity_id, user_id, scope, name, credential_ref, external_user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			identity_id      = EXCLUDED.identity_id,
			user_id          = EXCLUDED.user_id,
			scope            = EXCLUDED.scope,
			name             = EXCLUDED.name,
			credential_ref   = EXCLUDED.credential_ref,
			external_user_id = EXCLUDED.external_user_id
	`, a.ID, a.IdentityID, a.UserID, a.Scope, a.Name, a.CredentialRef, a.ExternalUserID)
	return err
}

func (s *Store) GetAppAuthorization(ctx context.Context, id string) (*domain.AppAuthorization, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, scope, name, credential_ref, external_user_id FROM app_authorizations WHERE id = $1`,
		id,
	)
	a, err := scanAppAuthorization(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

func (s *Store) GetAppAuthorizationByUserAndIdentity(ctx context.Context, userID, identityID string) (*domain.AppAuthorization, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, scope, name, credential_ref, external_user_id FROM app_authorizations WHERE user_id = $1 AND identity_id = $2`,
		userID, identityID,
	)
	a, err := scanAppAuthorization(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

func (s *Store) GetAppAuthorizationByExternalUser(ctx context.Context, identityID, externalUserID string) (*domain.AppAuthorization, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, identity_id, user_id, scope, name, credential_ref, external_user_id FROM app_authorizations WHERE identity_id = $1 AND external_user_id = $2`,
		identityID, externalUserID,
	)
	a, err := scanAppAuthorization(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return a, err
}

func (s *Store) ListAppAuthorizationsByUser(ctx context.Context, userID string) ([]*domain.AppAuthorization, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, identity_id, user_id, scope, name, credential_ref, external_user_id FROM app_authorizations WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.AppAuthorization
	for rows.Next() {
		a, err := scanAppAuthorization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAppAuthorization(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM app_authorizations WHERE id = $1`, id)
	return err
}

func scanAppAuthorization(row pgx.Row) (*domain.AppAuthorization, error) {
	var a domain.AppAuthorization
	if err := row.Scan(&a.ID, &a.IdentityID, &a.UserID, &a.Scope, &a.Name, &a.CredentialRef, &a.ExternalUserID); err != nil {
		return nil, err
	}
	return &a, nil
}
