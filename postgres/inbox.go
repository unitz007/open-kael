package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/unitz007/open-kael/domain"
)

var _ domain.InboxStore = (*Store)(nil)

// Insert stores an occurrence in the events inbox. A delivery stored before (same source and delivery id) is left as it
// was and reported as not inserted.
func (s *Store) Insert(ctx context.Context, o *domain.Occurrence) (int64, bool, error) {
	facts, err := json.Marshal(o.Facts)
	if err != nil {
		return 0, false, fmt.Errorf("postgres: encode facts: %w", err)
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO events_inbox (source, connection_id, delivery_id, event, facts, subject_kind, subject_value, repeat_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (source, delivery_id) DO NOTHING
		RETURNING id`,
		o.Source, o.ConnectionID, o.DeliveryID, o.Event, facts, o.SubjectKind, o.SubjectValue, o.RepeatKey).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("postgres: store event: %w", err)
	}
	return id, true, nil
}

// Counts says how many stored occurrences are in each status.
func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, COUNT(*) FROM events_inbox GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("postgres: count events: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
