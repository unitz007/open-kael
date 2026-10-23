package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// The agent list carries each agent's capabilities (with their connections, limits and abilities), so a creator's
// screen can tell which agents are ready to answer without opening each one; and only their own agents.
func TestListAgents_CarriesEachAgentsCapabilities(t *testing.T) {
	ctx := context.Background()
	store, _ := pgtest.Store(t)
	users := []string{"lst_a", "lst_b"}
	for _, u := range users {
		if err := store.SaveAccount(ctx, &domain.Account{ID: u, Email: u + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, AccountID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "lst_gh", Name: "GitHub", Service: "github"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "lst_prs", Name: "list_prs", IntegrationID: "lst_gh", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*domain.Agent{{ID: "lst_1", Name: "One", CreatedBy: "lst_a"}, {ID: "lst_2", Name: "Two", CreatedBy: "lst_a"}, {ID: "lst_3", Name: "Theirs", CreatedBy: "lst_b"}} {
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []*domain.Skill{
		{ID: "lst_s1", AgentID: "lst_1", Name: "prs", Integrations: []string{"lst_gh"}, LimitedIntegrations: []string{"lst_gh"}, Tools: []domain.ToolBinding{{ToolID: "lst_prs"}}},
		{ID: "lst_s2", AgentID: "lst_1", Name: "more", Integrations: []string{"lst_gh"}},
		{ID: "lst_s3", AgentID: "lst_3", Name: "theirs", Integrations: []string{"lst_gh"}},
	} {
		if err := store.SaveSkill(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	srv := api.NewServer(store)
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.Header.Set("Authorization", "Bearer tok_lst_a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var list []domain.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.Agent{}
	for _, a := range list {
		got[a.ID] = a
	}
	if _, ok := got["lst_3"]; ok || len(got) != 2 {
		t.Fatalf("only the creator's own agents: %v", list)
	}
	one := got["lst_1"].Skills
	if len(one) != 2 || one[0].ID != "lst_s1" || len(one[0].Tools) != 1 || one[0].Tools[0].ToolID != "lst_prs" || len(one[0].LimitedIntegrations) != 1 || len(one[1].Integrations) != 1 {
		t.Fatalf("each capability whole, as the agent's own page would load it: %+v", one)
	}
	if len(got["lst_2"].Skills) != 0 {
		t.Fatalf("an agent with no capabilities has none: %+v", got["lst_2"].Skills)
	}
}

// A capability's name is tidied when it is saved, and one that is far too long is refused.
func TestSkillTitle_IsTidiedAndKeptShort(t *testing.T) {
	ctx := context.Background()
	store, _ := pgtest.Store(t)
	if err := store.SaveAccount(ctx, &domain.Account{ID: "ttl_a", Email: "ttl_a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(ctx, &domain.Session{Token: "tok_ttl_a", AccountID: "ttl_a", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgent(ctx, &domain.Agent{ID: "ttl_ag", Name: "A", CreatedBy: "ttl_a"}); err != nil {
		t.Fatal(err)
	}
	srv := api.NewServer(store)
	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok_ttl_a")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(http.MethodPost, "/agents/ttl_ag/skills", `{"id":"ttl_s","name":"calendar_helper","title":"  Calendar \n  helper ","description":"Checks my calendar"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got, _ := store.GetSkill(ctx, "ttl_s"); got.Title != "Calendar helper" {
		t.Errorf("tidied: %q", got.Title)
	}
	long := strings.Repeat("x", domain.MaxSkillTitle+1)
	if rec := send(http.MethodPut, "/skills/ttl_s", `{"name":"calendar_helper","title":"`+long+`","description":"Checks my calendar"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("too long: %d", rec.Code)
	}
	if got, _ := store.GetSkill(ctx, "ttl_s"); got.Title != "Calendar helper" {
		t.Errorf("a refused change leaves it as it was: %q", got.Title)
	}
}
