package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
)

// skillStore serves one skill whose owning agent lookup can be made to fail.
type skillStore struct {
	noopStore
	agentErr error
	deleted  []string
	saved    int
}

func (s *skillStore) GetSkill(_ context.Context, id string) (*domain.Skill, error) {
	return &domain.Skill{ID: id, AgentID: "a1", Name: "x"}, nil
}
func (s *skillStore) GetAgent(_ context.Context, id string) (*domain.Agent, error) {
	if s.agentErr != nil {
		return nil, s.agentErr
	}
	return &domain.Agent{ID: id}, nil
}
func (s *skillStore) DeleteSkill(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}
func (s *skillStore) SaveSkill(_ context.Context, _ *domain.Skill) error { s.saved++; return nil }

func do(t *testing.T, st *skillStore, method, path, body string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	api.NewServer(st).ServeHTTP(rr, req)
	return rr.Code
}

// The ownership check must not be skipped when the owning agent can't be loaded.
func TestSkillEndpoints_FailClosedWhenOwnerCannotBeLoaded(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"agent missing":     {domain.ErrNotFound, http.StatusNotFound},
		"store unavailable": {errors.New("db down"), http.StatusInternalServerError},
	} {
		st := &skillStore{agentErr: tc.err}
		if got := do(t, st, http.MethodDelete, "/skills/s1", ""); got != tc.want || len(st.deleted) != 0 {
			t.Errorf("%s: DELETE got %d deleted=%v, want %d and nothing deleted", name, got, st.deleted, tc.want)
		}
		if got := do(t, st, http.MethodPut, "/skills/s1", `{"name":"y"}`); got != tc.want || st.saved != 0 {
			t.Errorf("%s: PUT got %d saved=%d, want %d and nothing saved", name, got, st.saved, tc.want)
		}
		if got := do(t, st, http.MethodGet, "/skills/s1", ""); got != tc.want {
			t.Errorf("%s: GET got %d, want %d", name, got, tc.want)
		}
	}
}

func TestSkillEndpoints_StillWorkForAccessibleAgents(t *testing.T) {
	st := &skillStore{}
	if got := do(t, st, http.MethodDelete, "/skills/s1", ""); got != http.StatusNoContent || len(st.deleted) != 1 {
		t.Errorf("DELETE got %d deleted=%v", got, st.deleted)
	}
	if got := do(t, st, http.MethodGet, "/skills/s1", ""); got != http.StatusOK {
		t.Errorf("GET got %d", got)
	}
	if got := do(t, st, http.MethodPut, "/skills/s1", `{"name":"y"}`); got != http.StatusOK || st.saved != 1 {
		t.Errorf("PUT got %d saved=%d", got, st.saved)
	}
}

// A limit to some of a connection's abilities only means something on a connection the capability uses.
func TestSkillEndpoints_ALimitMustBeOnAConnectionTheCapabilityUses(t *testing.T) {
	st := &skillStore{}
	post := func(body string) int { return do(t, st, http.MethodPost, "/agents/a1/skills", body) }
	if got := post(`{"id":"s1","name":"x","limited_integrations":["gmail"]}`); got != http.StatusBadRequest || st.saved != 0 {
		t.Errorf("a limit on a connection it does not list: %d saved=%d", got, st.saved)
	}
	if got := post(`{"id":"s1","name":"x","integrations":["calendar"],"limited_integrations":["gmail"]}`); got != http.StatusBadRequest || st.saved != 0 {
		t.Errorf("a limit on a different connection: %d saved=%d", got, st.saved)
	}
	if got := do(t, st, http.MethodPut, "/skills/s1", `{"name":"x","limited_integrations":["gmail"]}`); got != http.StatusBadRequest || st.saved != 0 {
		t.Errorf("the same on an update: %d saved=%d", got, st.saved)
	}
}

// tradingStore has one connection, its trading abilities, and keeps what is saved.
type tradingStore struct {
	skillStore
	last *domain.Skill
}

func (s *tradingStore) LoadIntegration(_ context.Context, id string) (*domain.Integration, error) {
	if id == "trading" {
		return &domain.Integration{ID: "trading", Service: "trading"}, nil
	}
	return nil, domain.ErrNotFound
}
func (s *tradingStore) ListToolsByIntegration(_ context.Context, id string) ([]*domain.ToolDefinition, error) {
	symbol := domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString}}}
	return []*domain.ToolDefinition{
		{ID: "t:status", Name: "get_trading_status", IntegrationID: "trading", ReadOnly: true, InputSchema: symbol},
		{ID: "t:place", Name: "place_trade", IntegrationID: "trading", InputSchema: symbol},
	}, nil
}
func (s *tradingStore) SaveSkill(_ context.Context, sk *domain.Skill) error {
	s.saved++
	s.last = sk
	return nil
}

// What must happen first is only between abilities the capability really has; a good one is saved tidied, with an id.
func TestSkillEndpoints_PrerequisitesAreHeldToWhatTheCapabilityCanDo(t *testing.T) {
	st := &tradingStore{}
	post := func(body string) int {
		rr := httptest.NewRecorder()
		api.NewServer(st).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/agents/a1/skills", strings.NewReader(body)))
		return rr.Code
	}
	for name, body := range map[string]string{
		"an ability it doesn't have":    `{"id":"s1","name":"x","integrations":["trading"],"prerequisites":[{"before":"t:place","first":"t:other"}]}`,
		"a change made to happen first": `{"id":"s1","name":"x","integrations":["trading"],"prerequisites":[{"before":"t:status","first":"t:place"}]}`,
		"an input one of them lacks":    `{"id":"s1","name":"x","integrations":["trading"],"prerequisites":[{"before":"t:place","first":"t:status","same":["size"]}]}`,
	} {
		if got := post(body); got != http.StatusBadRequest || st.saved != 0 {
			t.Errorf("%s: %d saved=%d", name, got, st.saved)
		}
	}
	if got := post(`{"id":"s1","name":"x","integrations":["trading"],"prerequisites":[{"before":"t:place","first":"t:status","same":["symbol"]},{"before":"t:place","first":"t:status","same":["symbol"]}]}`); got != http.StatusOK {
		t.Fatalf("a good one: %d", got)
	}
	if p := st.last.Prerequisites; len(p) != 1 || p[0].ID == "" || p[0].Same[0] != "symbol" {
		t.Errorf("saved once, with an id: %+v", p)
	}
	// Its own limits are held to the same: on an ability it has, a detail that ability takes, a value that fits.
	saved := st.saved
	for name, body := range map[string]string{
		"an ability it doesn't have": `{"id":"s1","name":"x","integrations":["trading"],"rules":[{"tool_id":"t:other","input":"symbol","kind":"one_of","value":["EURUSD"]}]}`,
		"a detail it doesn't take":   `{"id":"s1","name":"x","integrations":["trading"],"rules":[{"tool_id":"t:place","input":"size","kind":"max","value":5}]}`,
		"a number on text":           `{"id":"s1","name":"x","integrations":["trading"],"rules":[{"tool_id":"t:place","input":"symbol","kind":"max","value":5}]}`,
	} {
		if got := post(body); got != http.StatusBadRequest || st.saved != saved {
			t.Errorf("%s: %d", name, got)
		}
	}
	if got := post(`{"id":"s1","name":"x","integrations":["trading"],"rules":[{"tool_id":"t:place","input":"symbol","kind":"one_of","value":["EURUSD"]}]}`); got != http.StatusOK {
		t.Fatalf("a good limit: %d", got)
	}
	if r := st.last.Rules; len(r) != 1 || r[0].ID == "" {
		t.Errorf("saved with an id: %+v", r)
	}
}
