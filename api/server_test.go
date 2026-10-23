package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// newTestServer wires a real api.Server to a real Postgres database of the test's own (see
// postgres/pgtest): skip rather than fail when no database is available.
func newTestServer(t *testing.T) *api.Server {
	srv, _ := newTestServerWithStore(t)
	return srv
}

// newTestServerWithStore is newTestServer that also gives the store, for what the platform seeds
// (integrations and tools) and creators cannot create through the API.
func newTestServerWithStore(t *testing.T) (*api.Server, *postgres.Store) {
	t.Helper()
	store, _ := pgtest.Store(t)
	return api.NewServer(store), store
}

// seedIntegration puts in an integration (and tool) the way the platform does at start-up.
func seedIntegration(t *testing.T, store *postgres.Store, withTool bool) {
	t.Helper()
	ctx := context.Background()
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}); err != nil {
		t.Fatalf("seed integration: %v", err)
	}
	if withTool {
		if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "tool_1", IntegrationID: "int_1", Name: "create_pull_request", Action: "github.create_pull_request"}); err != nil {
			t.Fatalf("seed tool: %v", err)
		}
	}
}

// makeAgent creates an agent through the API. The server chooses its id, whatever the caller sends.
func makeAgent(t *testing.T, srv http.Handler, name string) string {
	t.Helper()
	rec := doJSON(t, srv, http.MethodPost, "/agents", domain.Agent{Name: name})
	if rec.Code != http.StatusOK {
		t.Fatalf("create agent: %d: %s", rec.Code, rec.Body.String())
	}
	var agent domain.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil || agent.ID == "" {
		t.Fatalf("decode agent: %v %+v", err, agent)
	}
	return agent.ID
}

func doJSON(t *testing.T, srv http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// Integrations and tools belong to the platform: creators can read them to choose what to connect,
// and cannot create or delete them.
func TestIntegrationEndpoints(t *testing.T) {
	srv, store := newTestServerWithStore(t)
	seedIntegration(t, store, true)

	rec := doJSON(t, srv, http.MethodGet, "/integrations/int_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "GitHub" || len(got.Tools) != 1 || got.Tools[0].ID != "tool_1" {
		t.Fatalf("unexpected integration (its tools are loaded with it): %+v", got)
	}

	rec = doJSON(t, srv, http.MethodGet, "/integrations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list []domain.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("unexpected list: %+v, err=%v", list, err)
	}

	if rec = doJSON(t, srv, http.MethodGet, "/integrations/does_not_exist", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", rec.Code)
	}
	if rec = doJSON(t, srv, http.MethodGet, "/integrations/int_1/tools", nil); rec.Code != http.StatusOK {
		t.Fatalf("list its tools: expected 200, got %d", rec.Code)
	}
	if rec = doJSON(t, srv, http.MethodGet, "/tools/tool_1", nil); rec.Code != http.StatusOK {
		t.Fatalf("get a tool: expected 200, got %d", rec.Code)
	}

	for name, req := range map[string]struct {
		method, path string
		body         any
	}{
		"create an integration": {http.MethodPost, "/integrations", domain.Integration{ID: "mine", Name: "Mine", Service: "x"}},
		"delete an integration": {http.MethodDelete, "/integrations/int_1", nil},
		"create a tool":         {http.MethodPost, "/integrations/int_1/tools", domain.ToolDefinition{ID: "t2", Name: "t"}},
	} {
		if rec := doJSON(t, srv, req.method, req.path, req.body); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("a creator cannot %s: expected 405, got %d", name, rec.Code)
		}
	}
	if rec = doJSON(t, srv, http.MethodGet, "/integrations/int_1", nil); rec.Code != http.StatusOK {
		t.Fatalf("and it is still there: %d", rec.Code)
	}
}

// TestNestedResourceEndpoints proves the pipeline through HTTP: a platform Integration with a Tool, an
// Agent a creator makes, and a Skill nested under that Agent binding the Tool — then confirms the
// parent GET shows the populated children (LoadAgent).
func TestNestedResourceEndpoints(t *testing.T) {
	srv, store := newTestServerWithStore(t)
	seedIntegration(t, store, true)
	agentID := makeAgent(t, srv, "Kael Dev")

	rec := doJSON(t, srv, http.MethodPost, "/agents/"+agentID+"/skills", domain.Skill{
		ID: "skill_1", Name: "ship_pr", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: "tool_1"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create skill: %d: %s", rec.Code, rec.Body.String())
	}
	var skill domain.Skill
	_ = json.Unmarshal(rec.Body.Bytes(), &skill)
	if skill.AgentID != agentID {
		t.Fatalf("expected skill's AgentID to be set from the URL, got %q", skill.AgentID)
	}

	rec = doJSON(t, srv, http.MethodGet, "/agents/"+agentID, nil)
	var agent domain.Agent
	_ = json.Unmarshal(rec.Body.Bytes(), &agent)
	if len(agent.Skills) != 1 || agent.Skills[0].ID != "skill_1" || len(agent.Skills[0].Tools) != 1 {
		t.Fatalf("expected agent.Skills populated with its tool binding, got %+v", agent.Skills)
	}
}

// A skill needs an id; an agent's id is the server's to make, whatever the caller sends.
func TestCreateWithoutID(t *testing.T) {
	srv := newTestServer(t)
	agentID := makeAgent(t, srv, "Kael Dev")

	rec := doJSON(t, srv, http.MethodPost, "/agents/"+agentID+"/skills", domain.Skill{Name: "no id"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a skill with no id, got %d", rec.Code)
	}

	rec = doJSON(t, srv, http.MethodPost, "/agents", domain.Agent{ID: "chosen_by_caller", Name: "Mine"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create agent: %d: %s", rec.Code, rec.Body.String())
	}
	var made domain.Agent
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if made.ID == "" || made.ID == "chosen_by_caller" {
		t.Fatalf("the server makes the agent's id: %q", made.ID)
	}
	if rec = doJSON(t, srv, http.MethodGet, "/agents/chosen_by_caller", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the id the caller sent is not used: %d", rec.Code)
	}
}

// TestEmptyListsReturnEmptyArray guards against Store.List* returning a nil
// slice encoding as JSON null instead of [] — a client shouldn't have to
// special-case "no results" as a different JSON type from "some results".
func TestEmptyListsReturnEmptyArray(t *testing.T) {
	srv, store := newTestServerWithStore(t)
	seedIntegration(t, store, false)
	agentID := makeAgent(t, srv, "Kael Dev")

	for _, path := range []string{"/integrations/int_1/tools", "/agents/" + agentID + "/skills"} {
		rec := doJSON(t, srv, http.MethodGet, path, nil)
		if got := rec.Body.String(); got != "[]\n" {
			t.Fatalf("%s: expected empty-array body \"[]\", got %q", path, got)
		}
	}
}
