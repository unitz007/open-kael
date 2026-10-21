package domain

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// EventHandler parses one specific event out of an already-verified webhook
// payload. ok=false means the payload is valid but does not represent this
// event — the runtime moves on to the next handler. err != nil means the
// payload is malformed and processing should stop.
//
// payload is the JSON object handed to whatever reacts to the event. It must
// satisfy the event's Payload schema (see the event contract). actorExternalID
// is the provider's identifier for the person the event is about (a GitHub
// login, a Stripe customer); empty when the event has no attributable person.
type EventHandler func(body []byte, header http.Header) (payload, actorExternalID string, ok bool, err error)

// IntegrationEvent declares one named event that an Integration can emit.
//
// An event is something that happened on a connection. It only tells: it never
// acts. Every event must be complete when it is registered; see the event
// contract in event_contract.go (ValidateEvent).
type IntegrationEvent struct {
	// Name is the canonical event name, e.g. "github.pull_request.opened".
	// Starts with the integration's service, unique across the platform, and
	// stable across deploys — stored rules refer to it by this string.
	Name string `json:"name"`

	// Description is one plain sentence a person would use ("A pull request
	// was opened"). It is shown wherever the event is offered, so it never uses
	// platform words (event, trigger, webhook, payload...).
	Description string `json:"description"`

	// Arrival says how the event reaches us.
	Arrival EventArrival `json:"arrival"`

	// Subject says who the event is about.
	Subject EventSubject `json:"subject"`

	// Payload describes what the agent is told when the event happens: an object
	// with typed, described properties and real required ones.
	Payload Schema `json:"payload"`

	// Options describes the settings a person may give when they ask to be
	// told about this event (which repository, how long before). May have no
	// properties.
	Options Schema `json:"options"`

	// RepeatKey names the required Payload property that identifies one
	// occurrence, so the same one is never handled twice.
	RepeatKey string `json:"repeat_key"`

	// Sample is one realistic payload. It must satisfy Payload.
	Sample map[string]any `json:"sample"`

	// Summary is an optional one-line message for a plain "tell me" alert, with blanks written {{name}}: each must be
	// a required, plain property of the payload, so the line can always be filled in. It is what lets an alert that
	// only tells be sent without a model writing it. An event without one can only be told by an agent. See
	// RenderSummary.
	Summary string `json:"summary,omitempty"`

	// Handler parses one incoming webhook payload for this event.
	// Not persisted — wired at startup alongside the Integration definition.
	// Required for pushed events.
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
	mu     sync.RWMutex // connections created while the server runs add and replace events, while agents read them
	events map[string][]*IntegrationEvent
}

func NewIntegrationEventRegistry() *IntegrationEventRegistry {
	return &IntegrationEventRegistry{events: make(map[string][]*IntegrationEvent)}
}

// Register records the events for a given service name (e.g. "github"). Every
// event is held to the event contract: one that breaks it is left out and
// reported in the returned error, and the rest are registered. An event name
// already registered by another service is refused too.
func (r *IntegrationEventRegistry) Register(service string, events []*IntegrationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := map[string]string{}
	for svc, evs := range r.events {
		if svc == service {
			continue
		}
		for _, e := range evs {
			taken[e.Name] = svc
		}
	}
	valid, err := ValidEvents(service, events)
	kept := valid[:0:0]
	var extra []error
	for _, e := range valid {
		if svc, dup := taken[e.Name]; dup {
			extra = append(extra, EventProblem{Event: e.Name, Message: fmt.Sprintf("is already registered by %q", svc)})
			continue
		}
		kept = append(kept, e)
	}
	r.events[service] = kept
	return errors.Join(append([]error{err}, extra...)...)
}

// EventsFor returns the events for the given service, or nil when none are registered.
func (r *IntegrationEventRegistry) EventsFor(service string) []*IntegrationEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*IntegrationEvent(nil), r.events[service]...)
}

// Remove forgets a service's events (a connection that was deleted).
func (r *IntegrationEventRegistry) Remove(service string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.events, service)
}

// ServiceOf says which integration's service an event belongs to.
func (r *IntegrationEventRegistry) ServiceOf(name string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for svc, evs := range r.events {
		for _, e := range evs {
			if e.Name == name {
				return svc, true
			}
		}
	}
	return "", false
}

// Services lists the services that have events, with their events, for whoever has to offer them.
func (r *IntegrationEventRegistry) Services() map[string][]*IntegrationEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]*IntegrationEvent, len(r.events))
	for svc, evs := range r.events {
		out[svc] = append([]*IntegrationEvent(nil), evs...)
	}
	return out
}

// Event finds a registered event by name.
func (r *IntegrationEventRegistry) Event(name string) (*IntegrationEvent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, evs := range r.events {
		for _, e := range evs {
			if e.Name == name {
				return e, true
			}
		}
	}
	return nil, false
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
const (
	IntegrationKindUser   = "user"   // creator-owned app (Slack, GitHub, Telegram, etc.)
	IntegrationKindSystem = "system" // platform-owned, seeded at startup
)

type Integration struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Service     string `json:"service"` // "github", "slack", "telegram", "discord", "google", etc.
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind,omitempty"` // IntegrationKindUser | IntegrationKindSystem; default "user"

	// CreatedBy is the ID of the creator who owns this integration. Empty for
	// platform-seeded integrations, which every creator can see. A creator's
	// own connection (see Source) is visible only to them.
	CreatedBy string `json:"created_by,omitempty"`

	// Source describes where a creator-built integration's tools come from.
	// Nil for platform-seeded integrations, whose tools are declared in code.
	Source *IntegrationSource `json:"source,omitempty"`

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

// Source types for a creator-built integration.
const (
	SourceOpenAPI = "openapi" // tools are the operations of an OpenAPI/Swagger description
	SourceMCP     = "mcp"     // tools are served by a remote MCP server
)

// Shared-key placements: how the single key is attached to every request.
const (
	AuthNone   = "none"
	AuthBearer = "bearer" // Authorization: Bearer <key>
	AuthHeader = "header" // <Name>: <key>
	AuthQuery  = "query"  // ?<Name>=<key>
	AuthBasic  = "basic"  // Authorization: Basic base64(<username>:<key>)
	// AuthOAuth: each person signs in to their own account with the standard
	// authorization-code handshake and calls go out as Authorization: Bearer
	// <their access token>. The creator's registered app (OAuthSettings) is
	// shared; the tokens are per person.
	AuthOAuth = "oauth"
)

// IntegrationSource is the non-secret description of a creator-built
// integration. The key itself is never stored here: it is the CredentialRef of
// the integration's token-kind Identity, encrypted at rest.
type IntegrationSource struct {
	Type string `json:"type"` // SourceOpenAPI | SourceMCP
	// URL is where the description was fetched from (the spec address) or, for
	// MCP, the server endpoint. Informational for OpenAPI; the call target is
	// BaseURL.
	URL string `json:"url"`
	// BaseURL is the root every OpenAPI operation path is appended to.
	BaseURL string     `json:"base_url,omitempty"`
	Auth    SourceAuth `json:"auth"`
}

// SourceAuth says how the shared key is sent.
type SourceAuth struct {
	Kind string `json:"kind"` // Auth* constant
	// Name is the header or query-parameter name for AuthHeader / AuthQuery.
	Name string `json:"name,omitempty"`
	// Username is the fixed user for AuthBasic; the key is the password.
	Username string `json:"username,omitempty"`
	// Personal means every person brings their own key instead of everyone
	// sharing one. The key is then stored as that person's AppAuthorization,
	// and for AuthBasic the person supplies the user name as well.
	Personal bool `json:"personal,omitempty"`
	// OAuth holds the creator's registered app when Kind is AuthOAuth. The
	// client secret is not here: it is the CredentialRef of the connection's
	// identity, encrypted.
	OAuth *OAuthSettings `json:"oauth,omitempty"`
}

// OAuthSettings is the non-secret part of an app the creator registered with the
// service so that people can sign in to their own accounts.
type OAuthSettings struct {
	AuthURL  string   `json:"auth_url"`  // where the person is sent to sign in
	TokenURL string   `json:"token_url"` // where a sign-in is exchanged for access
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes,omitempty"` // permissions to ask for
	// ClientAuthBasic sends the client id and secret as an HTTP Basic header when
	// exchanging a sign-in, instead of in the request body.
	ClientAuthBasic bool `json:"client_auth_basic,omitempty"`
}
