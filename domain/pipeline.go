package domain

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// The events pipeline (see closed-kael docs/events-pipeline.md).
//
// Only a small adapter per integration knows where an event comes from. It turns a delivery into an Occurrence, which
// is the event contract's terms (event, typed facts, who it is about, repeat key) whatever the source was. Everything
// after that point is the same for every source.

// Occurrence is one thing that happened, as the event contract describes it.
type Occurrence struct {
	Source       string         // the integration it came from, such as "github"
	ConnectionID string         // the identity it arrived through
	DeliveryID   string         // the source's own id for this delivery; unique together with Source
	Event        string         // IntegrationEvent.Name
	Facts        map[string]any // satisfies the event's Payload schema; the raw delivery is never kept
	SubjectKind  string         // what kind of identifier says who it is about, such as "github_account"
	SubjectValue string         // the identifier itself
	RepeatKey    string         // identifies this occurrence, so it is never handled twice
	ReceivedAt   time.Time
}

// Connection is the identity an incoming delivery belongs to and what is needed to check it.
type Connection struct {
	ID        string
	RouteKey  string // what a delivery says to find it: GitHub's app id
	Secret    []byte
	CreatorID string
}

// PushAdapter is the part of receiving an event that depends on the source: how to find which connection a delivery
// is for, check it really came from there, and read it. A source whose events are pushed to us (a webhook) has one.
type PushAdapter interface {
	// Source is the integration's name, and the address deliveries are posted to: /events/{Source}.
	Source() string
	// LegacyPaths are other addresses that deliveries already go to and must keep working.
	LegacyPaths() []string
	// RouteKey finds, from the request, the key that says which connection it is for. ok is false when it says none
	// (a repository webhook, as opposed to an app's); the connections are then tried one by one.
	RouteKey(r *http.Request, body []byte) (key string, ok bool)
	// Verify reports whether the delivery was signed with secret.
	Verify(secret []byte, r *http.Request, body []byte) bool
	// Decode reads a verified delivery. ok is false for a delivery that is not an event we handle (a ping).
	Decode(r *http.Request, body []byte) (occ *Occurrence, ok bool, err error)
}

// ConnectionResolver finds the connections a delivery may belong to. It may cache.
type ConnectionResolver interface {
	// Resolve returns the connections with this route key; with an empty key, every connection of the source that
	// has a secret. An unknown key returns none.
	Resolve(ctx context.Context, source, routeKey string) ([]*Connection, error)
}

// InboxStore keeps what arrived, durably, before anything is done with it.
type InboxStore interface {
	// Insert stores an occurrence. inserted is false when this delivery was stored before (a redelivery).
	Insert(ctx context.Context, o *Occurrence) (id int64, inserted bool, err error)
	// Counts says how many stored occurrences are in each status.
	Counts(ctx context.Context) (map[string]int, error)
}

// ExternalIdentity says that a person is known on a connection by an identifier the source uses: their GitHub login on
// a GitHub app, their id in a creator's service. An event that names such an identifier is about every person linked
// to it, which is why this is one to many (several people can be connected to one GitHub account, or to a shared app).
type ExternalIdentity struct {
	ConnectionID string // the identity (app) the identifier is seen through
	Kind         string // what sort of identifier: "github_account"; matches Occurrence.SubjectKind
	Value        string // the identifier, as NormalizeIdentifier makes it
	PersonID     string
	AuthID       string // the connected account it came from, when it came from one; deleting that deletes this
}

// IdentityKindEmail is the kind of identifier that is an email address. People are found by it through the verified
// emails of their logins, not through rows of their own, so it works for any connection and can only be someone who
// proved the address is theirs.
const IdentityKindEmail = "email"

// NormalizeIdentifier makes an identifier comparable. Sources that ignore case (GitHub logins, email addresses) are
// folded to lower case on both sides; every other kind is only trimmed.
func NormalizeIdentifier(kind, value string) string {
	value = strings.TrimSpace(value)
	switch kind {
	case "github_account", "email":
		return strings.ToLower(value)
	}
	return value
}

// PeopleStore finds the people an occurrence is about.
type PeopleStore interface {
	// Link records that a person is known by an identifier on a connection. Linking the same thing again is a no-op.
	Link(ctx context.Context, id ExternalIdentity) error
	// PeopleFor returns the people linked to an identifier on a connection, in a stable order; none when unknown.
	PeopleFor(ctx context.Context, connectionID, kind, value string) ([]string, error)
}

// How hard the pipeline tries before giving up on something it keeps failing at.
const (
	MaxAttempts  = 6
	backoffFirst = 30 * time.Second
	backoffLimit = time.Hour
)

// Backoff is how long to wait before trying again after the given number of attempts have failed: 30 seconds, doubling,
// never more than an hour.
func Backoff(attempts int) time.Duration {
	d := backoffFirst
	for i := 1; i < attempts && d < backoffLimit; i++ {
		d *= 2
	}
	if d > backoffLimit {
		d = backoffLimit
	}
	return d
}

// InboxRow is a stored occurrence that a worker holds a lease on.
type InboxRow struct {
	ID       int64
	Attempts int // including this one
	Occurrence
}

// InboxQueue is how workers take what is in the inbox. A row is leased for a while: only its holder may finish it,
// and if the holder dies the lease runs out and another worker takes it. Every finishing call reports whether it
// applied, which is false when the lease was lost; the caller must then not act as if it had finished.
type InboxQueue interface {
	// Lease takes up to limit rows that are due (new, or leased and the lease has run out) and holds them for hold.
	// Several workers may lease at once; they never get the same row. A row whose lease ran out after its last allowed
	// attempt is marked failed instead of handed out.
	Lease(ctx context.Context, worker string, limit int, hold time.Duration) ([]*InboxRow, error)
	// Done marks a row dealt with.
	Done(ctx context.Context, id int64, worker string) (bool, error)
	// Drop marks a row as not to be acted on (nobody to tell, no match), with the reason.
	Drop(ctx context.Context, id int64, worker, note string) (bool, error)
	// Fail records that a try failed: the row is tried again after retryAfter, or marked failed when it has used
	// MaxAttempts.
	Fail(ctx context.Context, id int64, worker, note string, retryAfter time.Duration) (bool, error)
}

// Levels say how much it costs to carry out a job (see closed-kael docs/events-pipeline.md, "Cost levels").
const (
	LevelNotify = "notify" // a message built from the facts; no model
	LevelFilter = "filter" // a quick yes or no from Jev, then a message built from the facts
	LevelAgent  = "agent"  // the agent's own model thinks or acts
)

// Job is one thing to do for one alert because of one occurrence. There is at most one per alert and repeat key, so a
// retry of the router cannot do anything twice.
type Job struct {
	ID        int64
	InboxID   int64 // the occurrence it came from; 0 when that has been cleared away
	AlertID   string
	PersonID  string
	AgentID   string
	Event     string
	RepeatKey string
	Facts     map[string]any
	Level     string
	RunAt     time.Time // not before this (quiet hours push it to the morning)
	Attempts  int       // including this one, once leased
	// MaxAttempts is how many times it may be started: a run that is interrupted (its worker stops before finishing) is
	// handed to another worker until it has been started this many times, then it is marked interrupted. 0 means
	// MaxAttempts (the package default). A job that may change something sets 1: it is never run a second time.
	MaxAttempts int
	// Shadow marks a job that is recorded but never carried out: it is kept with the status "shadow", is never leased,
	// and is counted against the caps only by other shadow jobs. It is how the router is run beside the original path
	// to see what it would do.
	Shadow bool
}

// What a cap on jobs counts.
const (
	CapAlert       = "alert"        // the jobs of one alert
	CapPersonAgent = "person_agent" // all the jobs of one person with one agent
)

// JobCap limits how many jobs may be made within a window of time.
type JobCap struct {
	Scope  string // CapAlert or CapPersonAgent
	Window time.Duration
	Max    int
}

// Outcomes of EnqueueCapped.
const (
	JobQueued    = "queued"
	JobDuplicate = "duplicate" // the alert already has a job for this occurrence
	JobOverCap   = "over_cap"
)

// EnqueueResult says what EnqueueCapped did. When Outcome is JobOverCap, Cap is the first cap that was full.
type EnqueueResult struct {
	Outcome string
	Cap     *JobCap
}

// JobStore keeps the jobs and hands them to workers, with the same lease rules as InboxQueue.
type JobStore interface {
	// Enqueue stores a job. inserted is false when the alert already has a job for this repeat key.
	Enqueue(ctx context.Context, j *Job) (inserted bool, err error)
	// EnqueueCapped stores a job unless the alert has one for this occurrence already, or a cap is full. The counting
	// and the storing are one step for each alert and for each person with an agent, so busy workers cannot together
	// go over a cap. Jobs that were dropped or failed do not count.
	EnqueueCapped(ctx context.Context, j *Job, caps []JobCap) (EnqueueResult, error)
	Lease(ctx context.Context, worker string, limit int, hold time.Duration) ([]*Job, error)
	// LeaseFair is Lease, but never gives one person more than perPerson jobs running at once, nor one agent more than
	// perAgent (0 means no limit on agents), counting those other workers hold, so one person or one busy agent cannot
	// take every worker. The counting and the taking are one step.
	LeaseFair(ctx context.Context, worker string, limit int, hold time.Duration, perPerson, perAgent int) ([]*Job, error)
	// Defer puts a leased job off until a later time without counting the attempt (quiet hours began while it waited).
	Defer(ctx context.Context, id int64, worker string, until time.Time) (bool, error)
	// Extend renews a lease the worker still holds, for a run that takes longer than the lease. It reports false when
	// the lease was lost (another worker has the job): the caller must stop and not report a result.
	Extend(ctx context.Context, id int64, worker string, hold time.Duration) (bool, error)
	// Release gives a job back unstarted, as at shutdown: it can be taken again at once and the attempt is not counted.
	Release(ctx context.Context, id int64, worker string) (bool, error)
	Done(ctx context.Context, id int64, worker string) (bool, error)
	Drop(ctx context.Context, id int64, worker, note string) (bool, error)
	// Fail records a failed try: the job is tried again after retryAfter, until it has been started MaxAttempts times,
	// then it is marked failed.
	Fail(ctx context.Context, id int64, worker, note string, retryAfter time.Duration) (bool, error)
	// FailFinal marks a job failed at once, whatever attempts it has left (an agent run that failed has already told
	// the person, so running it again would tell them twice).
	FailFinal(ctx context.Context, id int64, worker, note string) (bool, error)
	// TakeInterrupted returns jobs that were interrupted (a worker stopped while running them and they have no attempts
	// left), each only once, so the person can be told that something may not have finished.
	TakeInterrupted(ctx context.Context, limit int) ([]*Job, error)
	// Counts says how many jobs are in each status.
	Counts(ctx context.Context) (map[string]int, error)
}

// PipelinePurger applies the retention: what has been dealt with is not kept for ever, since facts hold text from
// outside (a pull request's title).
type PipelinePurger interface {
	// PurgeJobs deletes jobs that are done, dropped or shadow once older than keep, and failed or interrupted ones once
	// older than keepProblems. It returns how many.
	PurgeJobs(ctx context.Context, keep, keepProblems time.Duration) (int, error)
	// PurgeInbox does the same for stored occurrences (done or dropped; failed). Ones still waiting are never deleted.
	PurgeInbox(ctx context.Context, keep, keepProblems time.Duration) (int, error)
}
