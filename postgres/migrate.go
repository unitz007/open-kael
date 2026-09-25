// Package postgres is a concrete implementation of domain.Store backed by
// PostgreSQL. Tools must be reusable across many Skills by reference
// (ToolBinding), which forces a normalized, foreign-keyed structure;
// Postgres's jsonb columns still give the Schema-shaped fields
// (InputSchema/OutputSchema) the flexibility a document store would
// otherwise be chosen for. Other stores (SQLite, in-memory) can satisfy
// domain.Store without this package being imported.
package postgres

import (
	"context"
	_ "embed"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.Store = (*Store)(nil)

//go:embed schema.sql
var schemaSQL string

// Migrate creates every table this package needs, idempotently — safe to
// call on every startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	return err
}

// Store persists domain types to Postgres.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query),
// letting the scan* helpers in each file work for either a single-row or
// a multi-row read without duplicating the column list.
type rowScanner interface {
	Scan(dest ...any) error
}
