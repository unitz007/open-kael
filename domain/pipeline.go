package domain

import (
	"context"
	"net/http"
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
