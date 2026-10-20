package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
)

// readinessStore holds one agent that already has the chat app "old".
type readinessStore struct {
	noopStore
	saved          []string
	integrationFor map[string]string // identity id -> integration (and service) id; telegram unless said
}

func (s *readinessStore) LoadAgent(_ context.Context, id string) (*domain.Agent, error) {
	return &domain.Agent{ID: id, IdentityIDs: []string{"old"}}, nil
}
func (s *readinessStore) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	return s.LoadAgent(ctx, id)
}
func (s *readinessStore) GetAgentByIdentityID(context.Context, string) (*domain.Agent, error) {
	return nil, domain.ErrNotFound
}
func (s *readinessStore) GetIdentity(_ context.Context, id string) (*domain.Identity, error) {
	integration := s.integrationFor[id]
	if integration == "" {
		integration = "telegram"
	}
	return &domain.Identity{ID: id, IntegrationID: integration}, nil
}
func (s *readinessStore) LoadIntegration(_ context.Context, id string) (*domain.Integration, error) {
	return &domain.Integration{ID: id, Service: id}, nil
}
func (s *readinessStore) SaveAgent(_ context.Context, a *domain.Agent) error {
	s.saved = append(s.saved, strings.Join(a.IdentityIDs, ","))
	return nil
}

func (s *readinessStore) with(identity, integration string) *readinessStore {
	if s.integrationFor == nil {
		s.integrationFor = map[string]string{}
	}
	s.integrationFor[identity] = integration
	return s
}

func putIdentities(st *readinessStore, ready func(context.Context, *domain.Agent) string, body string) (int, string) {
	var opts []api.Option
	if ready != nil {
		opts = append(opts, api.WithAgentReadiness(ready))
	}
	rr := httptest.NewRecorder()
	api.NewServer(st, opts...).ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/agents/a1/identities", strings.NewReader(body)))
	return rr.Code, rr.Body.String()
}

func notReady(context.Context, *domain.Agent) string { return "Add a capability first." }
func isReady(context.Context, *domain.Agent) string  { return "" }

func TestAgentIdentities_AChatAppIsRefusedUntilTheAgentIsReady(t *testing.T) {
	st := &readinessStore{}
	code, body := putIdentities(st, notReady, `{"identity_ids":["old","new"]}`)
	if code != http.StatusConflict || !strings.Contains(body, "Add a capability first.") {
		t.Fatalf("got %d %s, want 409 with the reason", code, body)
	}
	if len(st.saved) != 0 {
		t.Errorf("nothing is saved when refused: %v", st.saved)
	}
}

func TestAgentIdentities_ReadyAgentsAndAgentsWithoutACheckAreNotHeldBack(t *testing.T) {
	for name, ready := range map[string]func(context.Context, *domain.Agent) string{"ready": isReady, "no check": nil} {
		st := &readinessStore{}
		if code, body := putIdentities(st, ready, `{"identity_ids":["old","new"]}`); code != http.StatusOK || len(st.saved) != 1 {
			t.Errorf("%s: got %d %s saved=%v", name, code, body, st.saved)
		}
	}
}

func TestAgentIdentities_TakingAChatAppAwayOrKeepingTheSameIsNeverRefused(t *testing.T) {
	for name, body := range map[string]string{"remove": `{"identity_ids":[]}`, "unchanged": `{"identity_ids":["old"]}`} {
		st := &readinessStore{}
		if code, b := putIdentities(st, notReady, body); code != http.StatusOK || len(st.saved) != 1 {
			t.Errorf("%s: got %d %s saved=%v", name, code, b, st.saved)
		}
	}
}

// The rule is about chat apps: a GitHub app (what listens for pull requests) is not held back by it.
func TestAgentIdentities_OnlyAChatAppIsHeldToTheRule(t *testing.T) {
	st := (&readinessStore{}).with("new", "github")
	if code, body := putIdentities(st, notReady, `{"identity_ids":["old","new"]}`); code != http.StatusOK {
		t.Fatalf("a GitHub identity is attached whether or not the agent is ready: %d %s", code, body)
	}
	st = (&readinessStore{}).with("new", "telegram")
	if code, _ := putIdentities(st, notReady, `{"identity_ids":["old","new"]}`); code != http.StatusConflict {
		t.Fatalf("a chat app is still refused: %d", code)
	}
}
