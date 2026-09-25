package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveIntegration(ctx context.Context, i *domain.Integration) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO integrations (id, name, service, description)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			service     = EXCLUDED.service,
			description = EXCLUDED.description
	`, i.ID, i.Name, i.Service, i.Description)
	return err
}

// getIntegrationRow fetches a bare Integration row (no Tools or Identities).
// Used internally by LoadIntegration (load.go) which populates the slices.
func (s *Store) getIntegrationRow(ctx context.Context, id string) (*domain.Integration, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, service, description FROM integrations WHERE id = $1
	`, id)
	i, err := scanIntegration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return i, err
}

func (s *Store) ListIntegrations(ctx context.Context) ([]*domain.Integration, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, service, description FROM integrations ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Integration
	for rows.Next() {
		i, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) DeleteIntegration(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM integrations WHERE id = $1`, id)
	return err
}

func scanIntegration(row rowScanner) (*domain.Integration, error) {
	var i domain.Integration
	if err := row.Scan(&i.ID, &i.Name, &i.Service, &i.Description); err != nil {
		return nil, err
	}
	return &i, nil
}
