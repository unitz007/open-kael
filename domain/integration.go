package domain

import "net/http"

// EventHandler parses one specific event out of an already-verified webhook
// payload. ok=false means the payload is valid but does not represent this
// event — the runtime moves on to the next handler. err != nil means the
// payload is malformed and processing should stop.
//
// payload is the structured string handed to the matching Skill as
// trigger_input. actorExternalID is the provider's identifier for the user
// who triggered the event (e.g. a GitHub login); empty when the event has no
// attributable actor (a bot push, a ping, an automated system event).
type EventHandler func(body []byte, header http.Header) (payload, actorExternalID string, ok bool, err error)

// IntegrationEvent declares one named event that an Integration can emit.
// Creators subscribe Skills to events by Name; the runtime calls Handler to
// confirm whether an incoming webhook payload represents this event and to
// extract its data.
type IntegrationEvent struct {
	// Name is the canonical event name, e.g. "github.pull_request.opened".
	// Must be unique within an Integration and stable across deploys — Skills
	// persist trigger subscriptions by this string.
	Name string `json:"name"`

	// Description is a short creator-facing explanation shown in the trigger
	// picker UI when a Skill is being configured.
	Description string `json:"description"`

	// Handler parses one incoming webhook payload for this event.
	// Not persisted — wired at startup alongside the Integration definition.
	Handler EventHandler `json:"-"`
}

// IntegrationEventRegistry maps integration service names (e.g. "github",
// "stripe") to their event catalogues. It is built at startup alongside the
// EventSourceRegistry and threaded into the API server so GET /integrations
// responses can include the events each integration can emit.
//
// Keyed by service name rather than integration ID because events are defined
// per-provider in code, not per-DB row — multiple GitHub App identities all
// emit the same set of events.
type IntegrationEventRegistry struct {
	events map[string][]*IntegrationEvent
}

func NewIntegrationEventRegistry() *IntegrationEventRegistry {
	return &IntegrationEventRegistry{events: make(map[string][]*IntegrationEvent)}
}

// Register records the events for a given service name (e.g. "github").
func (r *IntegrationEventRegistry) Register(service string, events []*IntegrationEvent) {
	r.events[service] = events
}

// EventsFor returns the events for the given service, or nil when none are registered.
func (r *IntegrationEventRegistry) EventsFor(service string) []*IntegrationEvent {
	return r.events[service]
}

// Integration is the top-level concept for one external service — the
// complete package for a provider (GitHub, Slack, Telegram, etc.). It groups
// the Identities (credentials/apps registered with that service), the Tools
// (capabilities the service exposes), and the Events it can emit via webhooks.
//
// Integrations are platform-owned: defined and seeded at startup by the
// platform operator. Creators pick existing integrations to build on; they
// do not create or delete them. Users connect to specific Identities within
// an Integration via AppAuthorization (API providers) or MessengerChannel
// (messaging providers) — never to the Integration directly.
type Integration struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Service     string `json:"service"` // "github", "slack", "telegram", "discord", "google", etc.
	Description string `json:"description,omitempty"`

	// Events is the set of named events this Integration can emit via its
	// webhook source. Skills subscribe to events by name; the runtime
	// dispatches an incoming webhook to the first matching IntegrationEvent.
	// Nil for integrations that emit no webhook events (e.g. Tavily).
	Events []*IntegrationEvent `json:"events,omitempty"`

	// Identities and Tools are denormalized convenience fields populated by
	// LoadIntegration / LoadAll — not persisted on this struct directly.
	Identities []*Identity       `json:"identities,omitempty"`
	Tools      []*ToolDefinition `json:"tools,omitempty"`
}
