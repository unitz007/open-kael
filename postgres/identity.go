package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveIdentity(ctx context.Context, i *domain.Identity) error {
	createdBy := i.CreatedBy
	var createdByArg interface{} = createdBy
	if createdBy == "" {
		createdByArg = nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO identities (id, integration_id, name, kind, app_id, credential_ref, webhook_secret_ref, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			integration_id     = EXCLUDED.integration_id,
			name               = EXCLUDED.name,
			kind               = EXCLUDED.kind,
			app_id             = EXCLUDED.app_id,
			credential_ref     = EXCLUDED.credential_ref,
			webhook_secret_ref = EXCLUDED.webhook_secret_ref,
			created_by         = EXCLUDED.created_by
	`, i.ID, i.IntegrationID, i.Name, i.Kind, i.AppID, i.CredentialRef, i.WebhookSecretRef, createdByArg)
	return err
}

func (s *Store) GetIdentity(ctx context.Context, id string) (*domain.Identity, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, integration_id, name, kind, app_id, credential_ref, webhook_secret_ref, COALESCE(created_by, '')
		FROM identities WHERE id = $1
	`, id)
	i, err := scanIdentity(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return i, err
}

// ListIdentities returns identities visible to ownerID: those created by
// ownerID plus platform-seeded identities (created_by IS NULL). Pass ownerID=""
// to return every identity without filtering (used at boot/runtime).
func (s *Store) ListIdentities(ctx context.Context, ownerID string) ([]*domain.Identity, error) {
	var pgrows pgx.Rows
	var err error
	if ownerID != "" {
		pgrows, err = s.pool.Query(ctx, `
			SELECT id, integration_id, name, kind, app_id, credential_ref, webhook_secret_ref, COALESCE(created_by, '')
			FROM identities WHERE (created_by = $1 OR created_by IS NULL) ORDER BY id
		`, ownerID)
	} else {
		pgrows, err = s.pool.Query(ctx, `
			SELECT id, integration_id, name, kind, app_id, credential_ref, webhook_secret_ref, COALESCE(created_by, '')
			FROM identities ORDER BY id
		`)
	}
	if err != nil {
		return nil, err
	}
	defer pgrows.Close()

	var out []*domain.Identity
	for pgrows.Next() {
		i, err := scanIdentity(pgrows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, pgrows.Err()
}

func (s *Store) ListIdentitiesByIntegration(ctx context.Context, integrationID string) ([]*domain.Identity, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, integration_id, name, kind, app_id, credential_ref, webhook_secret_ref, COALESCE(created_by, '')
		FROM identities WHERE integration_id = $1 ORDER BY id
	`, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) DeleteIdentity(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM identities WHERE id = $1`, id)
	return err
}

func scanIdentity(row rowScanner) (*domain.Identity, error) {
	var i domain.Identity
	if err := row.Scan(&i.ID, &i.IntegrationID, &i.Name, &i.Kind, &i.AppID, &i.CredentialRef, &i.WebhookSecretRef, &i.CreatedBy); err != nil {
		return nil, err
	}
	return &i, nil
}
