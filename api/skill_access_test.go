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
