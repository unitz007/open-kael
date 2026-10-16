package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveIntegration(ctx context.Context, i *domain.Integration) error {
	var source any
	if i.Source != nil {
		b, err := json.Marshal(i.Source)
		if err != nil {
			return fmt.Errorf("postgres: encode integration %q source: %w", i.ID, err)
		}
		source = b
	}
	var createdBy any
	if i.CreatedBy != "" {
		createdBy = i.CreatedBy
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO integrations (id, name, service, description, kind, created_by, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			service     = EXCLUDED.service,
			description = EXCLUDED.description,
			kind        = EXCLUDED.kind,
			created_by  = EXCLUDED.created_by,
			source      = EXCLUDED.source
	`, i.ID, i.Name, i.Service, i.Description, i.Kind, createdBy, source)
	return err
}

// getIntegrationRow fetches a bare Integration row (no Tools or Identities).
// Used internally by LoadIntegration (load.go) which populates the slices.
func (s *Store) getIntegrationRow(ctx context.Context, id string) (*domain.Integration, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, service, description, kind, created_by, source FROM integrations WHERE id = $1
	`, id)
	i, err := scanIntegration(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return i, err
}

func (s *Store) ListIntegrations(ctx context.Context) ([]*domain.Integration, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, service, description, kind, created_by, source FROM integrations ORDER BY id
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
	var createdBy *string
	var source []byte
	if err := row.Scan(&i.ID, &i.Name, &i.Service, &i.Description, &i.Kind, &createdBy, &source); err != nil {
		return nil, err
	}
	if createdBy != nil {
		i.CreatedBy = *createdBy
	}
	if len(source) > 0 {
		var src domain.IntegrationSource
		if err := json.Unmarshal(source, &src); err != nil {
			return nil, fmt.Errorf("postgres: decode integration %q source: %w", i.ID, err)
		}
		i.Source = &src
	}
	return &i, nil
}
