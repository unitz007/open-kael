package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// jobStore is the Store seen as domain.JobStore.
type jobStore struct{ s *Store }

var _ domain.JobStore = jobStore{}

// Jobs is where alerts' jobs are kept and taken from.
func (s *Store) Jobs() domain.JobStore { return jobStore{s} }

func validLevel(l string) bool {
	return l == domain.LevelNotify || l == domain.LevelFilter || l == domain.LevelAgent
}

// Enqueue stores a job; a second one for the same alert and repeat key is not stored.
func (j jobStore) Enqueue(ctx context.Context, job *domain.Job) (bool, error) {
	if job.AlertID == "" || job.PersonID == "" || job.AgentID == "" || job.Event == "" || job.RepeatKey == "" {
		return false, fmt.Errorf("postgres: enqueue job: need an alert, person, agent, event and repeat key")
	}
	if !validLevel(job.Level) {
		return false, fmt.Errorf("postgres: enqueue job: %q is not a level", job.Level)
	}
	facts, err := json.Marshal(job.Facts)
	if err != nil {
		return false, fmt.Errorf("postgres: encode job facts: %w", err)
	}
	var inbox any
	if job.InboxID != 0 {
		inbox = job.InboxID
	}
	runAt := job.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}
	err = j.s.pool.QueryRow(ctx, `
		INSERT INTO pipeline_jobs (inbox_id, alert_id, person_id, agent_id, event, repeat_key, facts, level, run_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (alert_id, repeat_key) DO NOTHING
		RETURNING id`,
		inbox, job.AlertID, job.PersonID, job.AgentID, job.Event, job.RepeatKey, facts, job.Level, runAt, startStatus(job)).Scan(&job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: enqueue job: %w", err)
	}
	return true, nil
}

func startStatus(j *domain.Job) string {
	if j.Shadow {
		return "shadow"
	}
	return "new"
}

// EnqueueCapped stores a job within caps. See domain.JobStore.
func (j jobStore) EnqueueCapped(ctx context.Context, job *domain.Job, caps []domain.JobCap) (domain.EnqueueResult, error) {
	if job.AlertID == "" || job.PersonID == "" || job.AgentID == "" || job.Event == "" || job.RepeatKey == "" || !validLevel(job.Level) {
		return domain.EnqueueResult{}, fmt.Errorf("postgres: enqueue job: need an alert, person, agent, event, repeat key and a level")
	}
	for _, c := range caps {
		if (c.Scope != domain.CapAlert && c.Scope != domain.CapPersonAgent) || c.Window <= 0 || c.Max < 0 {
			return domain.EnqueueResult{}, fmt.Errorf("postgres: enqueue job: not a cap: %+v", c)
		}
	}
	facts, err := json.Marshal(job.Facts)
	if err != nil {
		return domain.EnqueueResult{}, fmt.Errorf("postgres: encode job facts: %w", err)
	}
	runAt := job.RunAt
	if runAt.IsZero() {
		runAt = time.Now()
	}
	tx, err := j.s.pool.Begin(ctx)
	if err != nil {
		return domain.EnqueueResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// One lock per alert and one per person with an agent, always taken in the same order, so two workers counting
	// and storing for the same alert or person take turns instead of both seeing room.
	keys := []string{"job-alert:" + job.AlertID, "job-person-agent:" + job.PersonID + "|" + job.AgentID}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, k); err != nil {
			return domain.EnqueueResult{}, fmt.Errorf("postgres: lock for jobs: %w", err)
		}
	}
	var dup bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pipeline_jobs WHERE alert_id = $1 AND repeat_key = $2)`, job.AlertID, job.RepeatKey).Scan(&dup); err != nil {
		return domain.EnqueueResult{}, err
	}
	if dup {
		return domain.EnqueueResult{Outcome: domain.JobDuplicate}, nil
	}
	// A shadow job is counted only against other shadow jobs, and a real one only against real ones.
	statuses := []string{"new", "running", "done"}
	if job.Shadow {
		statuses = []string{"shadow"}
	}
	for i := range caps {
		c := caps[i]
		var n int
		var err error
		if c.Scope == domain.CapAlert {
			err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM pipeline_jobs WHERE alert_id = $1 AND status = ANY($2) AND created_at > NOW() - make_interval(secs => $3)`,
				job.AlertID, statuses, c.Window.Seconds()).Scan(&n)
		} else {
			err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM pipeline_jobs WHERE person_id = $1 AND agent_id = $2 AND status = ANY($3) AND created_at > NOW() - make_interval(secs => $4)`,
				job.PersonID, job.AgentID, statuses, c.Window.Seconds()).Scan(&n)
		}
		if err != nil {
			return domain.EnqueueResult{}, fmt.Errorf("postgres: count jobs: %w", err)
		}
		if n >= c.Max {
			return domain.EnqueueResult{Outcome: domain.JobOverCap, Cap: &caps[i]}, nil
		}
	}
	var inbox any
	if job.InboxID != 0 {
		inbox = job.InboxID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO pipeline_jobs (inbox_id, alert_id, person_id, agent_id, event, repeat_key, facts, level, run_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		inbox, job.AlertID, job.PersonID, job.AgentID, job.Event, job.RepeatKey, facts, job.Level, runAt, startStatus(job)).Scan(&job.ID); err != nil {
		return domain.EnqueueResult{}, fmt.Errorf("postgres: enqueue job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.EnqueueResult{}, err
	}
	return domain.EnqueueResult{Outcome: domain.JobQueued}, nil
}

// Lease takes jobs that are due and holds them.
func (j jobStore) Lease(ctx context.Context, worker string, limit int, hold time.Duration) ([]*domain.Job, error) {
	if limit <= 0 {
		return nil, nil
	}
	if _, err := j.s.pool.Exec(ctx, `
		UPDATE pipeline_jobs SET status = 'failed', locked_by = '', locked_until = NULL, finished_at = NOW(),
			note = 'gave up: the worker stopped ' || attempts || ' times before finishing it'
		WHERE status = 'running' AND locked_until < NOW() AND attempts >= $1`, domain.MaxAttempts); err != nil {
		return nil, fmt.Errorf("postgres: give up on jobs: %w", err)
	}
	rows, err := j.s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM pipeline_jobs
			WHERE (status = 'new' AND run_at <= NOW()) OR (status = 'running' AND locked_until < NOW())
			ORDER BY run_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		UPDATE pipeline_jobs p SET status = 'running', attempts = p.attempts + 1, locked_by = $1,
			locked_until = NOW() + make_interval(secs => $3)
		FROM due WHERE p.id = due.id
		RETURNING p.id, COALESCE(p.inbox_id, 0), p.alert_id, p.person_id, p.agent_id, p.event, p.repeat_key, p.facts, p.level, p.run_at, p.attempts`,
		worker, limit, hold.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgres: lease jobs: %w", err)
	}
	defer rows.Close()
	var out []*domain.Job
	for rows.Next() {
		job := &domain.Job{}
		var facts []byte
		if err := rows.Scan(&job.ID, &job.InboxID, &job.AlertID, &job.PersonID, &job.AgentID, &job.Event, &job.RepeatKey, &facts, &job.Level, &job.RunAt, &job.Attempts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(facts, &job.Facts); err != nil {
			return nil, fmt.Errorf("postgres: job %d has unreadable facts: %w", job.ID, err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (j jobStore) finish(ctx context.Context, id int64, worker, status, note string) (bool, error) {
	tag, err := j.s.pool.Exec(ctx, `
		UPDATE pipeline_jobs SET status = $3, note = $4, locked_by = '', locked_until = NULL, finished_at = NOW()
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`, id, worker, status, note)
	if err != nil {
		return false, fmt.Errorf("postgres: finish job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Done marks a leased job carried out.
func (j jobStore) Done(ctx context.Context, id int64, worker string) (bool, error) {
	return j.finish(ctx, id, worker, "done", "")
}

// Drop marks a leased job as not to be carried out, with the reason.
func (j jobStore) Drop(ctx context.Context, id int64, worker, note string) (bool, error) {
	return j.finish(ctx, id, worker, "dropped", note)
}

// Fail records a failed try: the job is tried again after retryAfter, or marked failed once it has used MaxAttempts.
func (j jobStore) Fail(ctx context.Context, id int64, worker, note string, retryAfter time.Duration) (bool, error) {
	tag, err := j.s.pool.Exec(ctx, `
		UPDATE pipeline_jobs SET
			status = CASE WHEN attempts >= $4 THEN 'failed' ELSE 'new' END,
			run_at = CASE WHEN attempts >= $4 THEN run_at ELSE NOW() + make_interval(secs => $5) END,
			finished_at = CASE WHEN attempts >= $4 THEN NOW() ELSE NULL END,
			note = $3, locked_by = '', locked_until = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`, id, worker, note, domain.MaxAttempts, retryAfter.Seconds())
	if err != nil {
		return false, fmt.Errorf("postgres: fail job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Counts says how many jobs are in each status.
func (j jobStore) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := j.s.pool.Query(ctx, `SELECT status, COUNT(*) FROM pipeline_jobs GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("postgres: count jobs: %w", err)
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
