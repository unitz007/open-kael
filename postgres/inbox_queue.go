package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// inboxQueue is the Store seen as domain.InboxQueue. It is a separate type because the inbox and the jobs both have
// Lease, Done, Drop and Fail.
type inboxQueue struct{ s *Store }

var _ domain.InboxQueue = inboxQueue{}

// InboxQueue is how workers take what is in the inbox.
func (s *Store) InboxQueue() domain.InboxQueue { return inboxQueue{s} }

// Lease takes inbox rows that are due and holds them. See domain.InboxQueue.
func (q inboxQueue) Lease(ctx context.Context, worker string, limit int, hold time.Duration) ([]*domain.InboxRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Whatever was being tried when its worker died, and has used every attempt, is given up on rather than tried again.
	if _, err := q.s.pool.Exec(ctx, `
		UPDATE events_inbox SET status = 'failed', locked_by = '', locked_until = NULL,
			note = 'gave up: the worker stopped ' || attempts || ' times before finishing it'
		WHERE status = 'routing' AND locked_until < NOW() AND attempts >= $1`, domain.MaxAttempts); err != nil {
		return nil, fmt.Errorf("postgres: give up on inbox rows: %w", err)
	}
	rows, err := q.s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM events_inbox
			WHERE (status = 'new' AND next_attempt_at <= NOW()) OR (status = 'routing' AND locked_until < NOW())
			ORDER BY next_attempt_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		UPDATE events_inbox e SET status = 'routing', attempts = e.attempts + 1, locked_by = $1,
			locked_until = NOW() + make_interval(secs => $3)
		FROM due WHERE e.id = due.id
		RETURNING e.id, e.attempts, e.source, e.connection_id, e.delivery_id, e.event, e.facts, e.subject_kind, e.subject_value, e.repeat_key, e.received_at`,
		worker, limit, hold.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgres: lease inbox: %w", err)
	}
	defer rows.Close()
	var out []*domain.InboxRow
	for rows.Next() {
		r := &domain.InboxRow{}
		var facts []byte
		if err := rows.Scan(&r.ID, &r.Attempts, &r.Source, &r.ConnectionID, &r.DeliveryID, &r.Event, &facts, &r.SubjectKind, &r.SubjectValue, &r.RepeatKey, &r.ReceivedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(facts, &r.Facts); err != nil {
			return nil, fmt.Errorf("postgres: inbox row %d has unreadable facts: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (q inboxQueue) finish(ctx context.Context, id int64, worker, status, note string) (bool, error) {
	tag, err := q.s.pool.Exec(ctx, `
		UPDATE events_inbox SET status = $3, note = $4, locked_by = '', locked_until = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'routing'`, id, worker, status, note)
	if err != nil {
		return false, fmt.Errorf("postgres: finish inbox row: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Done marks a leased row dealt with.
func (q inboxQueue) Done(ctx context.Context, id int64, worker string) (bool, error) {
	return q.finish(ctx, id, worker, "done", "")
}

// Drop marks a leased row as not to be acted on.
func (q inboxQueue) Drop(ctx context.Context, id int64, worker, note string) (bool, error) {
	return q.finish(ctx, id, worker, "dropped", note)
}

// Fail records a failed try on a leased row.
func (q inboxQueue) Fail(ctx context.Context, id int64, worker, note string, retryAfter time.Duration) (bool, error) {
	tag, err := q.s.pool.Exec(ctx, `
		UPDATE events_inbox SET
			status = CASE WHEN attempts >= $4 THEN 'failed' ELSE 'new' END,
			next_attempt_at = CASE WHEN attempts >= $4 THEN next_attempt_at ELSE NOW() + make_interval(secs => $5) END,
			note = $3, locked_by = '', locked_until = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'routing'`, id, worker, note, domain.MaxAttempts, retryAfter.Seconds())
	if err != nil {
		return false, fmt.Errorf("postgres: fail inbox row: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
