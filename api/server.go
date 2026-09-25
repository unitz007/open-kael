// Package api is the HTTP surface over the domain model — plain CRUD over a
// domain.Store for each core type, so Integrations, Tools, Agents, Skills,
// and Users can be managed by a UI or external client. Server accepts the
// domain.Store interface rather than a concrete postgres.Store, so the
// persistence layer is swappable. Routing uses the standard library's
// ServeMux method+path patterns (Go 1.22+), no router dependency.
//
// Provider-specific OAuth callbacks and other provider routes are registered
// through the RoutePlugin interface so server.go has no provider imports.
package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/unitz007/open-kael/domain"
)

type Server struct {
	store domain.Store
	mux   *http.ServeMux

	// authToken, when non-empty, requires every request to carry
	// "Authorization: Bearer <authToken>". Set via WithBearerAuth.
	authToken string

	// encryptor encrypts raw credentials before storing them. Required for
	// provider-specific identity creation endpoints. Set via
	// WithCredentialEncryptor; nil means those endpoints return 501.
	encryptor domain.CredentialEncryptor

	// integrationEvents, when non-nil, is attached to Integration responses so
	// the frontend can show the events each integration can emit without a
	// separate endpoint. Set via WithIntegrationEvents.
	integrationEvents *domain.IntegrationEventRegistry

	// onAgentIdentityChange, when set, is called after setAgentIdentities saves
	// an agent with a changed identity list. Receives the agent ID, the added
	// identity IDs, and the removed identity IDs. Used to start/stop listeners
	// without importing the runtime package.
	onAgentIdentityChange func(agentID string, addedIdentityIDs, removedIdentityIDs []string)

	// onSkillChange, when set, is called after a skill is created or deleted.
	// Receives the owning agent ID so the runtime can reload the hosted agent.
	onSkillChange func(agentID string)

	// routePlugins are provider-specific route registrars. Each plugin's Mount
	// is called from routes() after all generic routes are registered.
	routePlugins []RoutePlugin
}

// RouteContext is passed to RoutePlugin.Mount — it provides the shared server
// state a plugin needs to register routes and handle requests.
type RouteContext struct {
	Store       domain.Store
	Encryptor   domain.CredentialEncryptor
	UserAuth    func(http.HandlerFunc) http.HandlerFunc
	CreatorAuth func(http.HandlerFunc) http.HandlerFunc
}

// RoutePlugin registers provider-specific HTTP routes on the server's mux.
// Implementations are passed via WithRoutePlugin and called from routes()
// after all generic routes are registered.
type RoutePlugin interface {
	Mount(mux *http.ServeMux, ctx RouteContext)
}

type Option func(*Server)

// WithBearerAuth requires every request to carry "Authorization: Bearer
// <token>". Requests without it receive 401. No-op when token is empty —
// useful for local dev and tests where auth isn't needed.
func WithBearerAuth(token string) Option {
	return func(s *Server) { s.authToken = token }
}

// WithCredentialEncryptor enables the provider-specific identity creation
// endpoints (POST /identities/telegram, /identities/slack, etc.) by giving
// the server a way to encrypt raw credentials before storing them.
func WithCredentialEncryptor(enc domain.CredentialEncryptor) Option {
	return func(s *Server) { s.encryptor = enc }
}

// WithIntegrationEvents attaches an event catalogue to the server so
// GET /integrations responses include the events each integration can emit.
func WithIntegrationEvents(r *domain.IntegrationEventRegistry) Option {
	return func(s *Server) { s.integrationEvents = r }
}

// WithAgentIdentityChangeHook registers a callback invoked when
// PUT /agents/{id}/identities changes an agent's identity list. The callback
// receives the agent ID, the added identity IDs, and the removed identity IDs.
// Intended for wiring the runtime Host to start/stop listeners dynamically.
func WithAgentIdentityChangeHook(f func(agentID string, addedIdentityIDs, removedIdentityIDs []string)) Option {
	return func(s *Server) { s.onAgentIdentityChange = f }
}

// WithSkillChangeHook registers a callback invoked when a skill is created or
// deleted. The callback receives the owning agent ID. Intended for wiring the
// runtime Host to reload its in-memory agent when skills change.
func WithSkillChangeHook(f func(agentID string)) Option {
	return func(s *Server) { s.onSkillChange = f }
}

// WithRoutePlugin registers a RoutePlugin whose Mount is called from routes()
// after all generic routes, allowing providers to add OAuth callbacks and
// other provider-specific endpoints without touching server.go.
func WithRoutePlugin(p RoutePlugin) Option {
	return func(s *Server) { s.routePlugins = append(s.routePlugins, p) }
}

func NewServer(store domain.Store, opts ...Option) *Server {
	s := &Server{store: store, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

var errUnauthorized = errors.New("unauthorized")

// creatorAuth wraps a handler with creator-level auth. When API_AUTH_TOKEN is
// configured, the request must carry either the static token or a valid user
// session — this lets the browser-based creator UI log in with email/password
// while server-to-server callers keep using the static key. When no static
// token is configured the handler runs freely (dev default).
func (s *Server) creatorAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Always try to resolve a session user and inject into context — this
		// enables per-user agent scoping even when no static token is configured.
		if user, err := s.userFromRequest(r); err == nil {
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser{}, user)))
			return
		}
		if s.authToken == "" || bearerMatches(r, s.authToken) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, errUnauthorized)
	}
}

// userAuth wraps a handler with session-based auth — used on routes that
// end-users call once they are logged in. Stores the resolved *domain.User
// in the request context under ctxKeyUser so handlers can retrieve it via
// UserFromContext.
func (s *Server) userAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.userFromRequest(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyUser{}, user)))
	}
}

type ctxKeyUser struct{}

// UserFromContext retrieves the authenticated *domain.User from the request
// context. Returns ok=false when no session is present (static-token or
// unauthenticated dev mode).
func UserFromContext(ctx context.Context) (*domain.User, bool) {
	u, ok := ctx.Value(ctxKeyUser{}).(*domain.User)
	return u, ok
}

// userFromRequest resolves the bearer token in the Authorization header to a
// live, non-expired session and returns the owning User.
func (s *Server) userFromRequest(r *http.Request) (*domain.User, error) {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return nil, errUnauthorized
	}
	token := auth[len(prefix):]

	sess, err := s.store.GetSession(r.Context(), token)
	if err != nil || sess.Expired() {
		return nil, errUnauthorized
	}
	return s.store.GetUser(r.Context(), sess.UserID)
}

func bearerMatches(r *http.Request, token string) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(auth, prefix) && auth[len(prefix):] == token
}

func (s *Server) routes() {
	ca := s.creatorAuth // shorthand

	// Creator-only routes: platform operator manages agents, skills, tools.
	// Provider-specific identity creation — accept raw credentials, encrypt
	// server-side. The generic POST /identities is intentionally absent: the
	// caller must use the provider-specific endpoint so the server handles
	// credential encryption rather than requiring the caller to do it.
	s.mux.HandleFunc("POST /identities/telegram", ca(s.createTelegramIdentity))
	s.mux.HandleFunc("POST /identities/slack", ca(s.createSlackIdentity))
	s.mux.HandleFunc("POST /identities/github/app", ca(s.createGitHubAppIdentity))
	s.mux.HandleFunc("POST /identities/oauth", ca(s.createOAuthIdentity))
	s.mux.HandleFunc("GET /identities", ca(s.listIdentities))
	s.mux.HandleFunc("GET /identities/{id}", ca(s.getIdentity))
	s.mux.HandleFunc("DELETE /identities/{id}", ca(s.deleteIdentity))

	// Integrations and tools are platform-seeded — creators can read them to
	// pick what to connect identities and skills to, but cannot create or delete.
	s.mux.HandleFunc("GET /integrations", ca(s.listIntegrations))
	s.mux.HandleFunc("GET /integrations/{id}", ca(s.getIntegration))
	s.mux.HandleFunc("GET /integrations/{integrationID}/tools", ca(s.listToolsByIntegration))
	s.mux.HandleFunc("GET /tools/{id}", ca(s.getTool))

	s.mux.HandleFunc("POST /agents", ca(s.createAgent))
	s.mux.HandleFunc("GET /agents", ca(s.listAgents))
	s.mux.HandleFunc("GET /agents/{id}", ca(s.getAgent))
	s.mux.HandleFunc("DELETE /agents/{id}", ca(s.deleteAgent))
	s.mux.HandleFunc("PUT /agents/{id}/identities", ca(s.setAgentIdentities))

	s.mux.HandleFunc("POST /agents/{agentID}/skills", ca(s.createSkill))
	s.mux.HandleFunc("GET /agents/{agentID}/skills", ca(s.listSkillsByAgent))
	s.mux.HandleFunc("GET /skills/{id}", ca(s.getSkill))
	s.mux.HandleFunc("DELETE /skills/{id}", ca(s.deleteSkill))

	// User routes: public (register/login) and authenticated (me/logout).
	s.mux.HandleFunc("POST /users", s.registerUser)
	s.mux.HandleFunc("POST /sessions", s.loginUser)
	s.mux.HandleFunc("GET /users/me", s.userAuth(s.getMe))
	s.mux.HandleFunc("DELETE /sessions", s.userAuth(s.logoutUser))

	// User's AppAuthorizations: list connections the user has granted.
	s.mux.HandleFunc("GET /users/me/authorizations", s.userAuth(s.listMyAppAuthorizations))
	s.mux.HandleFunc("POST /users/me/authorizations", s.userAuth(s.createMyAppAuthorization))
	s.mux.HandleFunc("DELETE /users/me/authorizations/{id}", s.userAuth(s.deleteMyAppAuthorization))

	// User setup: agent + integrations the user needs to configure.
	s.mux.HandleFunc("GET /users/me/setup", s.userAuth(s.getMySetup))

	// User channels: redeem a bot-generated link code, list, or disconnect.
	s.mux.HandleFunc("POST /users/me/channels/redeem", s.userAuth(s.redeemChannelByCode))
	s.mux.HandleFunc("GET /users/me/channels", s.userAuth(s.listMyChannels))
	s.mux.HandleFunc("DELETE /users/me/channels/{id}", s.userAuth(s.deleteMyChannel))

	// Provider-specific route plugins (OAuth callbacks, provisioners, etc.)
	ctx := RouteContext{
		Store:       s.store,
		Encryptor:   s.encryptor,
		UserAuth:    s.userAuth,
		CreatorAuth: s.creatorAuth,
	}
	for _, p := range s.routePlugins {
		p.Mount(s.mux, ctx)
	}
}
