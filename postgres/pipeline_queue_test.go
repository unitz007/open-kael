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
	if c["interrupted"] != 1 {
		t.Fatalf("a job whose worker keeps stopping is interrupted, not failed: %v", c)
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

func enqueue(t *testing.T, j domain.JobStore, alert, person string, mutate func(*domain.Job)) *domain.Job {
	t.Helper()
	jb := job(alert, "r")
	jb.PersonID = person
	if mutate != nil {
		mutate(jb)
	}
	if ok, err := j.Enqueue(context.Background(), jb); err != nil || !ok {
		t.Fatalf("enqueue %s: %v %v", alert, ok, err)
	}
	return jb
}

// A run that takes longer than its lease renews it; a worker that lost the lease is told so.
func TestJobs_ExtendRenewsOnlyForTheHolder(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "a", "p1", nil)
	got, _ := j.Lease(ctx, "w1", 1, 200*time.Millisecond)
	id := got[0].ID
	if ok, _ := j.Extend(ctx, id, "intruder", time.Minute); ok {
		t.Fatal("only the holder renews")
	}
	if ok, err := j.Extend(ctx, id, "w1", time.Minute); err != nil || !ok {
		t.Fatalf("holder renews: %v %v", ok, err)
	}
	time.Sleep(350 * time.Millisecond)
	if again, _ := j.Lease(ctx, "w2", 1, time.Minute); len(again) != 0 {
		t.Fatal("a renewed lease did not run out")
	}
	// A lease that ran out and was taken by someone else cannot be renewed by the first worker.
	exec(t, pool, `UPDATE pipeline_jobs SET locked_until = NOW() - interval '1 second'`)
	if taken, _ := j.Lease(ctx, "w2", 1, time.Minute); len(taken) != 1 {
		t.Fatal("w2 takes the job whose lease ran out")
	}
	if ok, _ := j.Extend(ctx, id, "w1", time.Minute); ok {
		t.Fatal("w1 lost the lease and cannot renew it")
	}
}

// A released job goes back to the queue as if it had never been taken.
func TestJobs_ReleaseGivesItBackWithoutCountingTheAttempt(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "a", "p1", nil)
	got, _ := j.Lease(ctx, "w1", 1, time.Minute)
	if ok, _ := j.Release(ctx, got[0].ID, "intruder"); ok {
		t.Fatal("only the holder releases")
	}
	if ok, err := j.Release(ctx, got[0].ID, "w1"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	again, _ := j.Lease(ctx, "w2", 1, time.Minute)
	if len(again) != 1 || again[0].Attempts != 1 {
		t.Fatalf("taken again at once, still its first attempt: %+v", again)
	}
	if ok, _ := j.Release(ctx, got[0].ID, "w1"); ok {
		t.Fatal("w1 no longer holds it")
	}
}

// How many times a job may be started decides what an interruption means: a job that may change something is started
// once and never again.
func TestJobs_AnInterruptedRunIsRetriedOnlyIfItMay(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "once", "p1", func(x *domain.Job) { x.MaxAttempts = 1 })
	enqueue(t, j, "twice", "p1", func(x *domain.Job) { x.MaxAttempts = 2 })
	expire := func() {
		exec(t, pool, `UPDATE pipeline_jobs SET locked_until = NOW() - interval '1 second' WHERE status = 'running'`)
	}

	first, _ := j.Lease(ctx, "w1", 10, time.Minute)
	if len(first) != 2 {
		t.Fatalf("%d", len(first))
	}
	expire()
	second, _ := j.Lease(ctx, "w2", 10, time.Minute)
	if len(second) != 1 || second[0].AlertID != "twice" || second[0].Attempts != 2 || second[0].MaxAttempts != 2 {
		t.Fatalf("only the one that may be run again is: %+v", second)
	}
	expire()
	if third, _ := j.Lease(ctx, "w3", 10, time.Minute); len(third) != 0 {
		t.Fatalf("nor is it a third time: %+v", third)
	}
	c, _ := j.Counts(ctx)
	if c["interrupted"] != 2 || c["failed"] != 0 {
		t.Fatalf("both are interrupted, neither failed: %v", c)
	}

	// Each is reported once, so the person is told once.
	told, err := j.TakeInterrupted(ctx, 10)
	if err != nil || len(told) != 2 {
		t.Fatalf("%d %v", len(told), err)
	}
	if again, _ := j.TakeInterrupted(ctx, 10); len(again) != 0 {
		t.Fatalf("not told twice: %d", len(again))
	}
}

func TestJobs_TakeInterruptedFindsThemEvenIfNoWorkerLeasesFirst(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "a", "p1", func(x *domain.Job) { x.MaxAttempts = 1 })
	j.Lease(ctx, "w1", 1, time.Minute)
	exec(t, pool, `UPDATE pipeline_jobs SET locked_until = NOW() - interval '1 second'`)
	told, err := j.TakeInterrupted(ctx, 5)
	if err != nil || len(told) != 1 || told[0].AlertID != "a" {
		t.Fatalf("%+v %v", told, err)
	}
}

func TestJobs_FailRetriesUntilTheJobsOwnLimitAndFailFinalEndsAtOnce(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "limited", "p1", func(x *domain.Job) { x.MaxAttempts = 2 })
	for i := 1; i <= 2; i++ {
		got, _ := j.Lease(ctx, "w", 1, time.Minute)
		if len(got) != 1 || got[0].Attempts != i {
			t.Fatalf("try %d: %+v", i, got)
		}
		if ok, _ := j.Fail(ctx, got[0].ID, "w", "busy", 0); !ok {
			t.Fatal("fail applies")
		}
	}
	if c, _ := j.Counts(ctx); c["failed"] != 1 {
		t.Fatalf("it had two tries: %v", c)
	}
	enqueue(t, j, "final", "p1", nil)
	got, _ := j.Lease(ctx, "w", 1, time.Minute)
	if ok, _ := j.FailFinal(ctx, got[0].ID, "intruder", "x"); ok {
		t.Fatal("only the holder")
	}
	if ok, _ := j.FailFinal(ctx, got[0].ID, "w", "the agent could not finish"); !ok {
		t.Fatal("holder ends it")
	}
	if again, _ := j.Lease(ctx, "w", 1, time.Minute); len(again) != 0 {
		t.Fatal("a job that failed for good is not leased again, though it had tries left")
	}
	if c, _ := j.Counts(ctx); c["failed"] != 2 {
		t.Fatalf("%v", c)
	}
}

func TestJobs_AJobMayNotAskForMoreAttemptsThanThereAre(t *testing.T) {
	s, _ := world(t)
	j := s.Jobs()
	person(t, s, "p1")
	for _, n := range []int{-1, domain.MaxAttempts + 1} {
		jb := job("a", "r")
		jb.MaxAttempts = n
		if _, err := j.Enqueue(context.Background(), jb); err == nil {
			t.Errorf("%d attempts is refused", n)
		}
		if _, err := j.EnqueueCapped(context.Background(), jb, nil); err == nil {
			t.Errorf("%d attempts is refused when capped too", n)
		}
	}
}

// No person has more than their share running at once, whoever asks, and nobody is left out by someone else's burst.
func TestJobs_LeaseFairKeepsOnePersonFromTakingEveryWorker(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "busy")
	person(t, s, "quiet")
	for i := 0; i < 6; i++ {
		jb := job(fmt.Sprint("b", i), "r")
		jb.PersonID = "busy"
		jb.RunAt = time.Now().Add(-time.Duration(10-i) * time.Minute) // older than the quiet person's
		if ok, err := j.Enqueue(ctx, jb); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	enqueue(t, j, "q0", "quiet", nil)

	got, err := j.LeaseFair(ctx, "w1", 10, time.Minute, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	per := map[string]int{}
	for _, g := range got {
		per[g.PersonID]++
	}
	if per["busy"] != 2 || per["quiet"] != 1 || len(got) != 3 {
		t.Fatalf("two for the busy one, one for the quiet one: %v", per)
	}
	more, _ := j.LeaseFair(ctx, "w2", 10, time.Minute, 2, 0)
	if len(more) != 0 {
		t.Fatalf("the busy person is at their share while those are running: %d", len(more))
	}
	// When one finishes, the next of theirs is taken.
	j.Done(ctx, got[0].ID, "w1")
	next, _ := j.LeaseFair(ctx, "w2", 10, time.Minute, 2, 0)
	if len(next) != 1 || next[0].PersonID != "busy" {
		t.Fatalf("%+v", next)
	}
}

func TestJobs_LeaseFairHoldsUnderManyWorkers(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	for p := 0; p < 3; p++ {
		person(t, s, fmt.Sprint("p", p))
		for i := 0; i < 8; i++ {
			jb := job(fmt.Sprintf("a%d-%d", p, i), "r")
			jb.PersonID = fmt.Sprint("p", p)
			if ok, err := j.Enqueue(ctx, jb); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := j.LeaseFair(ctx, fmt.Sprint("w", w), 5, time.Minute, 2, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	rows, _ := pool.Query(ctx, `SELECT person_id, COUNT(*) FROM pipeline_jobs WHERE status = 'running' GROUP BY person_id`)
	defer rows.Close()
	total := 0
	for rows.Next() {
		var p string
		var n int
		_ = rows.Scan(&p, &n)
		total += n
		if n > 2 {
			t.Errorf("%s has %d running", p, n)
		}
	}
	if total != 6 {
		t.Errorf("each of three people has their two: %d running", total)
	}
}

// What has been dealt with is deleted after its time; what is waiting, running or recent is not.
func TestPurge_KeepsWhatIsStillNeededAndDeletesTheRest(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	storeInbox(t, s, 6)
	exec(t, pool, `UPDATE events_inbox SET status = 'done', received_at = NOW() - interval '40 days' WHERE delivery_id = 'd000'`)
	exec(t, pool, `UPDATE events_inbox SET status = 'dropped', received_at = NOW() - interval '31 days' WHERE delivery_id = 'd001'`)
	exec(t, pool, `UPDATE events_inbox SET status = 'failed', received_at = NOW() - interval '40 days' WHERE delivery_id = 'd002'`)
	exec(t, pool, `UPDATE events_inbox SET status = 'done', received_at = NOW() - interval '1 day' WHERE delivery_id = 'd003'`)
	exec(t, pool, `UPDATE events_inbox SET status = 'new', received_at = NOW() - interval '90 days' WHERE delivery_id = 'd004'`) // still waiting
	exec(t, pool, `UPDATE events_inbox SET status = 'failed', received_at = NOW() - interval '100 days' WHERE delivery_id = 'd005'`)
	keep, problems := 30*24*time.Hour, 90*24*time.Hour
	n, err := s.PurgeInbox(ctx, keep, problems)
	if err != nil || n != 3 {
		t.Fatalf("done 40d, dropped 31d and failed 100d go: %d %v", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM events_inbox`).Scan(&left)
	if left != 3 {
		t.Fatalf("a recent one, a waiting one and a failed one under 90 days stay: %d", left)
	}

	j := s.Jobs()
	for i, st := range []string{"done", "dropped", "shadow", "failed", "interrupted", "new", "running", "done"} {
		jb := job(fmt.Sprint("p", i), "r")
		if ok, err := j.Enqueue(ctx, jb); err != nil || !ok {
			t.Fatal(ok, err)
		}
		age := "40 days"
		if st == "failed" || st == "interrupted" {
			age = "40 days" // older than 30 but under 90: kept
		}
		if i == 7 {
			age = "1 day" // a recent one
		}
		exec(t, pool, fmt.Sprintf(`UPDATE pipeline_jobs SET status = '%s', created_at = NOW() - interval '%s', finished_at = NOW() - interval '%s' WHERE alert_id = 'p%d'`, st, age, age, i))
	}
	exec(t, pool, `UPDATE pipeline_jobs SET finished_at = NOW() - interval '120 days', created_at = NOW() - interval '120 days' WHERE alert_id = 'p4'`) // interrupted, very old
	n, err = s.PurgeJobs(ctx, keep, problems)
	if err != nil || n != 4 {
		t.Fatalf("done, dropped, shadow (40d) and the interrupted one (120d) go: %d %v", n, err)
	}
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM pipeline_jobs`).Scan(&left)
	if left != 4 {
		t.Fatalf("a failed 40d, a new, a running and a recent done stay: %d", left)
	}
}

// Choosing who runs next is one step at a time: while one worker is choosing, another waits its turn.
func TestJobs_LeaseFairWorkersTakeTurns(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	enqueue(t, s.Jobs(), "a", "p1", nil)
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx) //nolint:errcheck
	if _, err := hold.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('job-lease', 0))`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Jobs().LeaseFair(ctx, "w", 1, time.Minute, 2, 0)
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("it chose while another worker was choosing")
	case <-time.After(300 * time.Millisecond):
	}
	_ = hold.Rollback(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("it did not go ahead once the other had finished")
	}
}

// A job that was started and interrupted may have told the person, so it still counts against the limits.
func TestJobs_AnInterruptedJobStillCountsAgainstTheCaps(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "a", "p1", nil)
	exec(t, pool, `UPDATE pipeline_jobs SET status = 'interrupted'`)
	res, err := j.EnqueueCapped(ctx, job("a", "r2"), []domain.JobCap{{Scope: domain.CapAlert, Window: time.Hour, Max: 1}})
	if err != nil || res.Outcome != domain.JobOverCap {
		t.Fatalf("%+v %v", res, err)
	}
}

// No agent has more than its share running at once either, whoever the people are.
func TestJobs_LeaseFairAlsoLimitsWhatOneAgentRuns(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	for p := 0; p < 4; p++ {
		person(t, s, fmt.Sprint("p", p))
		enqueue(t, j, fmt.Sprint("busyagent", p), fmt.Sprint("p", p), func(x *domain.Job) { x.AgentID = "A" })
		enqueue(t, j, fmt.Sprint("otheragent", p), fmt.Sprint("p", p), func(x *domain.Job) { x.AgentID = "B"; x.RepeatKey = "r2" })
	}
	got, err := j.LeaseFair(ctx, "w1", 20, time.Minute, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	per := map[string]int{}
	for _, g := range got {
		per[g.AgentID]++
	}
	if per["A"] != 2 || per["B"] != 2 {
		t.Fatalf("two for each agent, not all of them: %v", per)
	}
	if more, _ := j.LeaseFair(ctx, "w2", 20, time.Minute, 5, 2); len(more) != 0 {
		t.Fatalf("both agents are at their share while those run: %d", len(more))
	}
	unlimited, _ := j.LeaseFair(ctx, "w3", 20, time.Minute, 5, 0)
	if len(unlimited) != 4 {
		t.Fatalf("0 means no limit on agents: %d", len(unlimited))
	}
}

// A deferred job waits for its new time and is not counted as an attempt.
func TestJobs_DeferPutsItOffWithoutCountingTheAttempt(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	j := s.Jobs()
	person(t, s, "p1")
	enqueue(t, j, "a", "p1", nil)
	got, _ := j.Lease(ctx, "w1", 1, time.Minute)
	if ok, _ := j.Defer(ctx, got[0].ID, "intruder", time.Now().Add(time.Hour)); ok {
		t.Fatal("only the holder")
	}
	if ok, err := j.Defer(ctx, got[0].ID, "w1", time.Now().Add(150*time.Millisecond)); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if again, _ := j.Lease(ctx, "w2", 1, time.Minute); len(again) != 0 {
		t.Fatal("not before its time")
	}
	time.Sleep(250 * time.Millisecond)
	again, _ := j.Lease(ctx, "w2", 1, time.Minute)
	if len(again) != 1 || again[0].Attempts != 1 {
		t.Fatalf("at its time, still its first attempt: %+v", again)
	}
}
