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
}

// JobStore keeps the jobs and hands them to workers, with the same lease rules as InboxQueue.
type JobStore interface {
	// Enqueue stores a job. inserted is false when the alert already has a job for this repeat key.
	Enqueue(ctx context.Context, j *Job) (inserted bool, err error)
	Lease(ctx context.Context, worker string, limit int, hold time.Duration) ([]*Job, error)
	Done(ctx context.Context, id int64, worker string) (bool, error)
	Drop(ctx context.Context, id int64, worker, note string) (bool, error)
	Fail(ctx context.Context, id int64, worker, note string, retryAfter time.Duration) (bool, error)
	// Counts says how many jobs are in each status.
	Counts(ctx context.Context) (map[string]int, error)
}
