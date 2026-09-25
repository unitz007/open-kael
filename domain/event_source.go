package domain

import "net/http"

// EventSource is the HTTP transport side of the event bus — it owns one HTTP
// path and knows how to verify the signature on incoming webhook deliveries.
// It says nothing about which events a payload represents; that is the job of
// IntegrationEvent.Handler, which the runtime calls after verification.
//
// Separating transport (EventSource) from semantics (IntegrationEvent) means
// adding a new event type to an integration never touches the HTTP layer, and
// the same WebhookSource implementation can serve every identity of a given
// integration — only the secret changes.
//
// No implementation ships in this package — GitHub, Stripe, Linear, and every
// other sender each have their own path and signature scheme.
type EventSource interface {
	// Path is the HTTP route this source registers on, e.g. "/webhooks/github".
	Path() string

	// Verify checks the raw body against the request's signature header(s).
	// Returns false when the header is absent, malformed, or the signature
	// does not match — the HTTP handler rejects the delivery with 401.
	Verify(body []byte, header http.Header) bool
}

// EventSourceEntry pairs an EventSource with the Identity that owns it and
// the Integration's event catalogue. The runtime verifies the delivery with
// Source, then walks Events to find the matching handler and canonical name.
type EventSourceEntry struct {
	IdentityID string
	Source     EventSource
	Events     []*IntegrationEvent
}

// EventSourceRegistry maps HTTP paths to EventSourceEntries — the same role
// ExecutorRegistry plays for Executors, keyed by path instead of service name.
type EventSourceRegistry struct {
	sources map[string]EventSourceEntry
}

func NewEventSourceRegistry() *EventSourceRegistry {
	return &EventSourceRegistry{sources: make(map[string]EventSourceEntry)}
}

// Register adds source to the registry, keyed by its path. identityID is the
// Identity whose webhook secret backs this source; events is the Integration's
// event catalogue — walked in order for every verified delivery.
func (r *EventSourceRegistry) Register(identityID string, s EventSource, events []*IntegrationEvent) {
	r.sources[s.Path()] = EventSourceEntry{IdentityID: identityID, Source: s, Events: events}
}

func (r *EventSourceRegistry) ForPath(path string) (EventSourceEntry, bool) {
	e, ok := r.sources[path]
	return e, ok
}

// All returns every registered entry — used by RegisterEventSources to wire
// HTTP handlers for all sources in one pass.
func (r *EventSourceRegistry) All() []EventSourceEntry {
	out := make([]EventSourceEntry, 0, len(r.sources))
	for _, e := range r.sources {
		out = append(out, e)
	}
	return out
}
