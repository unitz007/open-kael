package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

func storeInbox(t *testing.T, s *postgres.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		d := fmt.Sprintf("d%03d", i)
		if _, ok, err := s.Insert(context.Background(), &domain.Occurrence{Source: "q", ConnectionID: "c", DeliveryID: d, Event: "q.thing", Facts: map[string]any{"n": float64(i)}, SubjectKind: "k", SubjectValue: "v", RepeatKey: d}); err != nil || !ok {
			t.Fatalf("store %s: %v %v", d, ok, err)
		}
	}
}

func inboxCounts(t *testing.T, s *postgres.Store) map[string]int {
	t.Helper()
	c, err := s.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Workers lease what is due, never the same row twice, and what comes back is what was stored.
func TestInboxQueue_LeasesWhatIsDueAndNeverTwice(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	storeInbox(t, s, 5)
	exec(t, pool, `UPDATE events_inbox SET next_attempt_at = NOW() + interval '1 hour' WHERE delivery_id = 'd004'`)

	first, err := q.Lease(ctx, "w1", 3, time.Minute)
	if err != nil || len(first) != 3 {
		t.Fatalf("limit is respected: %d %v", len(first), err)
	}
	if first[0].Attempts != 1 || first[0].Facts["n"] == nil || first[0].Source != "q" || first[0].SubjectValue != "v" || first[0].ReceivedAt.IsZero() {
		t.Fatalf("the stored occurrence comes back whole: %+v", first[0])
	}
	second, _ := q.Lease(ctx, "w2", 10, time.Minute)
	if len(second) != 1 {
		t.Fatalf("one more is due (the leased are held, one is not due yet): %d", len(second))
	}
	seen := map[int64]bool{}
	for _, r := range append(first, second...) {
		if seen[r.ID] {
			t.Fatalf("row %d leased twice", r.ID)
		}
		seen[r.ID] = true
	}
	if more, _ := q.Lease(ctx, "w3", 10, time.Minute); len(more) != 0 {
		t.Fatalf("nothing is due: %d", len(more))
	}
}

// Many workers at once share the rows out without overlap or loss.
func TestInboxQueue_ConcurrentWorkersDivideTheRows(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	const total = 200
	storeInbox(t, s, total)

	var mu sync.Mutex
	got := map[int64]string{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		worker := fmt.Sprintf("w%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rows, err := q.Lease(ctx, worker, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					if prev, dup := got[r.ID]; dup {
						t.Errorf("row %d leased by %s and %s", r.ID, prev, worker)
					}
					got[r.ID] = worker
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != total {
		t.Fatalf("every row was leased once: %d of %d", len(got), total)
	}
}

// Only the holder of a lease can finish a row. If a worker stops, its lease runs out and another takes the row; the
// first one's late answer is refused.
func TestInboxQueue_LeaseBelongsToItsHolderUntilItRunsOut(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	storeInbox(t, s, 1)

	rows, _ := q.Lease(ctx, "slow", 1, 50*time.Millisecond)
	id := rows[0].ID
	if ok, _ := q.Done(ctx, id, "intruder"); ok {
		t.Fatal("someone else cannot finish it")
	}
	if again, _ := q.Lease(ctx, "other", 1, time.Minute); len(again) != 0 {
		t.Fatal("it is held, so it is not handed out")
	}
	time.Sleep(120 * time.Millisecond)
	taken, _ := q.Lease(ctx, "other", 1, time.Minute)
	if len(taken) != 1 || taken[0].ID != id || taken[0].Attempts != 2 {
		t.Fatalf("after the lease ran out another worker has it, on its second attempt: %+v", taken)
	}
	if ok, _ := q.Done(ctx, id, "slow"); ok {
		t.Fatal("the worker that lost the lease is refused")
	}
	if ok, err := q.Done(ctx, id, "other"); err != nil || !ok {
		t.Fatalf("the new holder finishes it: %v %v", ok, err)
	}
	if ok, _ := q.Done(ctx, id, "other"); ok {
		t.Fatal("finishing twice is refused")
	}
	if c := inboxCounts(t, s); c["done"] != 1 || c["routing"] != 0 {
		t.Fatalf("%v", c)
	}
}

func TestInboxQueue_DropRecordsWhy(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	storeInbox(t, s, 1)
	rows, _ := q.Lease(ctx, "w", 1, time.Minute)
	if ok, err := q.Drop(ctx, rows[0].ID, "w", "nobody to tell"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var status, note string
	if err := pool.QueryRow(ctx, `SELECT status, note FROM events_inbox WHERE id = $1`, rows[0].ID).Scan(&status, &note); err != nil || status != "dropped" || note != "nobody to tell" {
		t.Fatalf("%q %q %v", status, note, err)
	}
}

// A failed try is tried again later, and given up on after MaxAttempts.
func TestInboxQueue_FailRetriesThenGivesUp(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	storeInbox(t, s, 1)

	rows, _ := q.Lease(ctx, "w", 1, time.Minute)
	if ok, _ := q.Fail(ctx, rows[0].ID, "w", "database busy", time.Hour); !ok {
		t.Fatal("fail applies")
	}
	if again, _ := q.Lease(ctx, "w", 1, time.Minute); len(again) != 0 {
		t.Fatal("it waits for the retry time")
	}
	for attempt := 2; attempt <= domain.MaxAttempts; attempt++ { // the first try failed above
		rows, _ = q.Lease(ctx, "w", 1, time.Minute) // retry time is past once we pull it forward
		if len(rows) == 0 {
			// pull the retry forward, as time would
			exec(t, pool, `UPDATE events_inbox SET next_attempt_at = NOW()`)
			rows, _ = q.Lease(ctx, "w", 1, time.Minute)
		}
		if len(rows) != 1 {
			t.Fatalf("attempt %d: nothing to lease", attempt)
		}
		if ok, _ := q.Fail(ctx, rows[0].ID, "w", "still failing", 0); !ok {
			t.Fatal("fail applies")
		}
	}
	if c := inboxCounts(t, s); c["failed"] != 1 || c["new"] != 0 {
		t.Fatalf("after %d attempts it is given up on: %v", domain.MaxAttempts, c)
	}
	if again, _ := q.Lease(ctx, "w", 1, time.Minute); len(again) != 0 {
		t.Fatal("a failed row is not leased again")
	}
}

// A row whose worker keeps dying is given up on instead of being handed out for ever.
func TestInboxQueue_ARowThatKeepsKillingItsWorkerIsGivenUp(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	q := s.InboxQueue()
	storeInbox(t, s, 1)
	for i := 0; i < domain.MaxAttempts; i++ {
		rows, _ := q.Lease(ctx, "w", 1, 10*time.Millisecond)
		if len(rows) != 1 {
			t.Fatalf("attempt %d should be handed out", i+1)
		}
		time.Sleep(40 * time.Millisecond) // the worker never answers
	}
	if rows, _ := q.Lease(ctx, "w", 1, time.Minute); len(rows) != 0 {
		t.Fatal("it is not handed out a seventh time")
	}
	if c := inboxCounts(t, s); c["failed"] != 1 {
		t.Fatalf("%v", c)
	}
}

func TestBackoff_DoublesAndStops(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got := domain.Backoff(i + 1); got != w {
			t.Errorf("after %d: %v, want %v", i+1, got, w)
		}
	}
	if domain.Backoff(0) != 30*time.Second || domain.Backoff(50) != time.Hour {
		t.Errorf("%v %v", domain.Backoff(0), domain.Backoff(50))
	}
}

func job(alert, repeat string) *domain.Job {
	return &domain.Job{AlertID: alert, PersonID: "p1", AgentID: "a1", Event: "q.thing", RepeatKey: repeat, Facts: map[string]any{"title": "Fix it"}, Level: domain.LevelNotify}
}

// One job per alert and occurrence; a bad one is refused; one for later is not taken yet.
func TestJobs_OneAlertOneOccurrenceOneJob(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")

	a := job("alert1", "r1")
	if ok, err := j.Enqueue(ctx, a); err != nil || !ok || a.ID == 0 {
		t.Fatalf("stored: %v %v", ok, err)
	}
	if ok, _ := j.Enqueue(ctx, job("alert1", "r1")); ok {
		t.Fatal("the same alert and occurrence is not stored twice")
	}
	if ok, _ := j.Enqueue(ctx, job("alert2", "r1")); !ok {
		t.Fatal("another alert gets its own job for the same occurrence")
	}
	if ok, _ := j.Enqueue(ctx, job("alert1", "r2")); !ok {
		t.Fatal("the same alert gets another for another occurrence")
	}
	bad := job("alert3", "r1")
	bad.Level = "shout"
	if _, err := j.Enqueue(ctx, bad); err == nil {
		t.Fatal("an unknown level is refused")
	}
	if _, err := j.Enqueue(ctx, &domain.Job{AlertID: "x", Level: domain.LevelNotify}); err == nil {
		t.Fatal("an incomplete job is refused")
	}

	later := job("alert9", "r1")
	later.RunAt = time.Now().Add(time.Hour)
	if ok, _ := j.Enqueue(ctx, later); !ok {
		t.Fatal("stored for later")
	}
	got, err := j.Lease(ctx, "w", 10, time.Minute)
	if err != nil || len(got) != 3 {
		t.Fatalf("the three due, not the one for the morning: %d %v", len(got), err)
	}
	for _, g := range got {
		if g.AlertID == "alert9" || g.Attempts != 1 || g.Facts["title"] != "Fix it" || g.Level != domain.LevelNotify {
			t.Fatalf("%+v", g)
		}
	}
}

// Jobs follow the same lease rules as the inbox.
func TestJobs_LeaseFinishFailAndGiveUp(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	for i := 0; i < 3; i++ {
		if ok, err := j.Enqueue(ctx, job(fmt.Sprint("a", i), "r")); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	got, _ := j.Lease(ctx, "w", 3, time.Minute)
	if len(got) != 3 {
		t.Fatal(len(got))
	}
	if ok, _ := j.Done(ctx, got[0].ID, "intruder"); ok {
		t.Fatal("only the holder finishes a job")
	}
	if ok, _ := j.Done(ctx, got[0].ID, "w"); !ok {
		t.Fatal("holder finishes")
	}
	if ok, _ := j.Drop(ctx, got[1].ID, "w", "quiet hours passed"); !ok {
		t.Fatal("holder drops")
	}
	if ok, _ := j.Fail(ctx, got[2].ID, "w", "model busy", time.Hour); !ok {
		t.Fatal("holder fails")
	}
	c, _ := j.Counts(ctx)
	if c["done"] != 1 || c["dropped"] != 1 || c["new"] != 1 {
		t.Fatalf("%v", c)
	}
	if again, _ := j.Lease(ctx, "w", 3, time.Minute); len(again) != 0 {
		t.Fatal("the failed one waits for its retry time")
	}

	// A job whose worker keeps dying is given up on.
	exec(t, pool, `UPDATE pipeline_jobs SET run_at = NOW() WHERE status = 'new'`)
	for i := 0; i < domain.MaxAttempts-1; i++ {
		rows, _ := j.Lease(ctx, "w", 1, 10*time.Millisecond)
		if len(rows) != 1 {
			t.Fatalf("attempt %d", i+2)
		}
		time.Sleep(40 * time.Millisecond)
	}
	if rows, _ := j.Lease(ctx, "w", 1, time.Minute); len(rows) != 0 {
		t.Fatal("not handed out beyond the limit")
	}
	c, _ = j.Counts(ctx)
	if c["failed"] != 1 {
		t.Fatalf("%v", c)
	}
}

// Jobs belong to a person, so a merge keeps them with the one who remains.
func TestJobs_MoveOnAMerge(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "from")
	person(t, s, "to")
	j := s.Jobs()
	x := job("a", "r")
	x.PersonID = "from"
	if ok, err := j.Enqueue(ctx, x); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.MergePersons(ctx, "from", "to"); err != nil {
		t.Fatal(err)
	}
	var who string
	if err := pool.QueryRow(ctx, `SELECT person_id FROM pipeline_jobs`).Scan(&who); err != nil || who != "to" {
		t.Fatalf("%q %v", who, err)
	}
}

// A row another worker is in the middle of taking is skipped, not waited for.
func TestInboxQueue_DoesNotWaitForARowSomeoneElseHolds(t *testing.T) {
	s, pool := world(t)
	storeInbox(t, s, 3)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM events_inbox ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := s.InboxQueue().Lease(waitCtx, "w", 10, time.Minute)
	if err != nil || len(rows) != 2 {
		t.Fatalf("the two free rows come back at once: %d %v", len(rows), err)
	}
}

// A job that keeps failing is given up on after MaxAttempts.
func TestJobs_FailingJobIsGivenUpOn(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	if ok, err := j.Enqueue(ctx, job("a", "r")); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for i := 1; i <= domain.MaxAttempts; i++ {
		rows, _ := j.Lease(ctx, "w", 1, time.Minute)
		if len(rows) != 1 || rows[0].Attempts != i {
			t.Fatalf("attempt %d: %+v", i, rows)
		}
		if ok, _ := j.Fail(ctx, rows[0].ID, "w", "model down", 0); !ok {
			t.Fatal("fail applies")
		}
	}
	c, _ := j.Counts(ctx)
	if c["failed"] != 1 || c["new"] != 0 {
		t.Fatalf("%v", c)
	}
	if rows, _ := j.Lease(ctx, "w", 1, time.Minute); len(rows) != 0 {
		t.Fatal("not tried again")
	}
}

func capped(t *testing.T, j domain.JobStore, jb *domain.Job, caps ...domain.JobCap) string {
	t.Helper()
	res, err := j.EnqueueCapped(context.Background(), jb, caps)
	if err != nil {
		t.Fatal(err)
	}
	return res.Outcome
}

// A cap is a limit on jobs made in a window, per alert or per person with an agent; a repeat is a duplicate, not a cap.
func TestJobs_CapsPerAlertAndPerPersonWithAnAgent(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	perAlert := domain.JobCap{Scope: domain.CapAlert, Window: time.Hour, Max: 2}
	perPerson := domain.JobCap{Scope: domain.CapPersonAgent, Window: 24 * time.Hour, Max: 3}

	for i, want := range []string{domain.JobQueued, domain.JobQueued, domain.JobOverCap} {
		if got := capped(t, j, job("a1", fmt.Sprint("r", i)), perAlert, perPerson); got != want {
			t.Fatalf("job %d: %s, want %s", i, got, want)
		}
	}
	if got := capped(t, j, job("a1", "r0"), perAlert, perPerson); got != domain.JobDuplicate {
		t.Fatalf("the same occurrence again is a duplicate, not a cap: %s", got)
	}
	res, _ := j.EnqueueCapped(ctx, job("a1", "r9"), []domain.JobCap{perAlert, perPerson})
	if res.Cap == nil || res.Cap.Scope != domain.CapAlert {
		t.Fatalf("it says which cap was full: %+v", res)
	}
	if got := capped(t, j, job("a2", "r0"), perAlert, perPerson); got != domain.JobQueued {
		t.Fatalf("another alert has its own count: %s", got)
	}
	res, _ = j.EnqueueCapped(ctx, job("a3", "r0"), []domain.JobCap{perAlert, perPerson})
	if res.Outcome != domain.JobOverCap || res.Cap.Scope != domain.CapPersonAgent {
		t.Fatalf("three in all for this person with this agent: %+v", res)
	}
	other := job("a4", "r0")
	other.AgentID = "a2"
	if got := capped(t, j, other, perAlert, perPerson); got != domain.JobQueued {
		t.Fatalf("another agent has its own count: %s", got)
	}

	// Old ones fall out of the window, and ones that were dropped or failed never counted.
	exec(t, pool, `UPDATE pipeline_jobs SET created_at = NOW() - interval '2 hours' WHERE alert_id = 'a1'`)
	if got := capped(t, j, job("a1", "r10"), perAlert); got != domain.JobQueued {
		t.Fatalf("an hour has passed: %s", got)
	}
	exec(t, pool, `UPDATE pipeline_jobs SET status = 'dropped' WHERE alert_id = 'a2'`)
	for i := 0; i < 3; i++ {
		if got := capped(t, j, job("a2", fmt.Sprint("n", i)), domain.JobCap{Scope: domain.CapAlert, Window: time.Hour, Max: 3}); got != domain.JobQueued {
			t.Fatalf("dropped jobs do not count: %s", got)
		}
	}
}

// Busy workers together never go over a cap.
func TestJobs_CapsHoldWhenManyWorkersEnqueueAtOnce(t *testing.T) {
	s, pool := world(t)
	j := s.Jobs()
	person(t, s, "p1")
	var queued int
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 60; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := j.EnqueueCapped(context.Background(), job(fmt.Sprint("a", i%3), fmt.Sprint("r", i)),
				[]domain.JobCap{{Scope: domain.CapPersonAgent, Window: time.Hour, Max: 5}})
			if err != nil {
				t.Error(err)
				return
			}
			if res.Outcome == domain.JobQueued {
				mu.Lock()
				queued++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM pipeline_jobs`).Scan(&n)
	if queued != 5 || n != 5 {
		t.Fatalf("exactly the cap: queued=%d stored=%d", queued, n)
	}
}

// A shadow job is kept and counted among shadow jobs only; it is never taken by a worker.
func TestJobs_ShadowJobsAreRecordedAndNeverRun(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	cap1 := domain.JobCap{Scope: domain.CapAlert, Window: time.Hour, Max: 1}
	if got := capped(t, j, job("a1", "r0"), cap1); got != domain.JobQueued {
		t.Fatal(got)
	}
	sh := job("a1", "r1")
	sh.Shadow = true
	if got := capped(t, j, sh, cap1); got != domain.JobQueued {
		t.Fatal(got)
	}
	sh2 := job("a1", "r2")
	sh2.Shadow = true
	if got := capped(t, j, sh2, cap1); got != domain.JobOverCap {
		t.Fatalf("shadow jobs have their own count: %s", got)
	}
	if got := capped(t, j, job("a1", "r3"), cap1); got != domain.JobOverCap {
		t.Fatalf("a real job counts real jobs only, and the shadow ones did not use up room: %s", got)
	}
	got, _ := j.Lease(ctx, "w", 10, time.Minute)
	if len(got) != 1 || got[0].RepeatKey != "r0" {
		t.Fatalf("only the real one is taken: %+v", got)
	}
	if c, _ := j.Counts(ctx); c["shadow"] != 1 || c["running"] != 1 {
		t.Fatalf("%v", c)
	}
}

// Counting and storing for one alert are a single step: while another worker is in the middle of it, a worker waits its turn.
func TestJobs_WorkersTakeTurnsPerAlert(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx) //nolint:errcheck
	if _, err := hold.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('job-alert:a1', 0))`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Jobs().EnqueueCapped(ctx, job("a1", "r1"), []domain.JobCap{{Scope: domain.CapAlert, Window: time.Hour, Max: 1}})
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("it went ahead while another worker held the alert")
	case <-time.After(300 * time.Millisecond):
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("it did not go ahead once the alert was free")
	}
}
