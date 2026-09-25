package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// newTestServer wires a real api.Server to a real Postgres via
// DATABASE_URL — same integration-test-not-unit-test posture as
// postgres/store_test.go: skip rather than fail when no database is
// available.
func newTestServer(t *testing.T) *api.Server {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping api integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable at DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Truncate both before AND after — see postgres/store_test.go's
	// newTestStore for why (a prior manual/demo session's leftover rows in
	// this same database shouldn't make an unrelated test's assertions
	// flaky).
	truncate := func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE skill_tools, skills, tools, agents, integrations CASCADE`)
	}
	truncate()
	t.Cleanup(truncate)

	return api.NewServer(postgres.New(pool))
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

func TestIntegrationEndpoints(t *testing.T) {
	srv := newTestServer(t)

	rec := doJSON(t, srv, http.MethodPost, "/integrations", domain.Integration{
		ID: "int_1", Name: "GitHub", Service: "github",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, srv, http.MethodGet, "/integrations/int_1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got domain.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "GitHub" {
		t.Fatalf("unexpected integration: %+v", got)
	}

	rec = doJSON(t, srv, http.MethodGet, "/integrations", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list []domain.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("unexpected list: %+v, err=%v", list, err)
	}

	rec = doJSON(t, srv, http.MethodGet, "/integrations/does_not_exist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: expected 404, got %d", rec.Code)
	}

	rec = doJSON(t, srv, http.MethodDelete, "/integrations/int_1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", rec.Code)
	}
	rec = doJSON(t, srv, http.MethodGet, "/integrations/int_1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rec.Code)
	}
}

// TestNestedResourceEndpoints proves the full pipeline through HTTP: create
// an Integration, a Tool nested under it, an Agent, and a Skill nested
// under that Agent binding the Tool — then confirm the parent GETs show
// the populated children (LoadIntegration/LoadAgent).
func TestNestedResourceEndpoints(t *testing.T) {
	srv := newTestServer(t)

	if rec := doJSON(t, srv, http.MethodPost, "/integrations", domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}); rec.Code != http.StatusOK {
		t.Fatalf("create integration: %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, srv, http.MethodPost, "/integrations/int_1/tools", domain.ToolDefinition{
		ID: "tool_1", Name: "create_pull_request", Action: "github.create_pull_request",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create tool: %d: %s", rec.Code, rec.Body.String())
	}
	var tool domain.ToolDefinition
	_ = json.Unmarshal(rec.Body.Bytes(), &tool)
	if tool.IntegrationID != "int_1" {
		t.Fatalf("expected tool's IntegrationID to be set from the URL, got %q", tool.IntegrationID)
	}

	rec = doJSON(t, srv, http.MethodGet, "/integrations/int_1", nil)
	var integration domain.Integration
	_ = json.Unmarshal(rec.Body.Bytes(), &integration)
	if len(integration.Tools) != 1 || integration.Tools[0].ID != "tool_1" {
		t.Fatalf("expected integration.Tools populated, got %+v", integration.Tools)
	}

	if rec := doJSON(t, srv, http.MethodPost, "/agents", domain.Agent{ID: "agent_1", Name: "Kael Dev"}); rec.Code != http.StatusOK {
		t.Fatalf("create agent: %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, srv, http.MethodPost, "/agents/agent_1/skills", domain.Skill{
		ID: "skill_1", Name: "ship_pr", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: "tool_1"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create skill: %d: %s", rec.Code, rec.Body.String())
	}
	var skill domain.Skill
	_ = json.Unmarshal(rec.Body.Bytes(), &skill)
	if skill.AgentID != "agent_1" {
		t.Fatalf("expected skill's AgentID to be set from the URL, got %q", skill.AgentID)
	}

	rec = doJSON(t, srv, http.MethodGet, "/agents/agent_1", nil)
	var agent domain.Agent
	_ = json.Unmarshal(rec.Body.Bytes(), &agent)
	if len(agent.Skills) != 1 || agent.Skills[0].ID != "skill_1" || len(agent.Skills[0].Tools) != 1 {
		t.Fatalf("expected agent.Skills populated with its tool binding, got %+v", agent.Skills)
	}
}

func TestCreateWithoutID(t *testing.T) {
	srv := newTestServer(t)

	rec := doJSON(t, srv, http.MethodPost, "/integrations", domain.Integration{Name: "no id"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing id, got %d", rec.Code)
	}
}

// TestEmptyListsReturnEmptyArray guards against Store.List* returning a nil
// slice encoding as JSON null instead of [] — a client shouldn't have to
// special-case "no results" as a different JSON type from "some results".
func TestEmptyListsReturnEmptyArray(t *testing.T) {
	srv := newTestServer(t)

	if rec := doJSON(t, srv, http.MethodPost, "/integrations", domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}); rec.Code != http.StatusOK {
		t.Fatalf("create integration: %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/agents", domain.Agent{ID: "agent_1", Name: "Kael Dev"}); rec.Code != http.StatusOK {
		t.Fatalf("create agent: %d: %s", rec.Code, rec.Body.String())
	}

	for _, path := range []string{"/integrations/int_1/tools", "/agents/agent_1/skills"} {
		rec := doJSON(t, srv, http.MethodGet, path, nil)
		if got := rec.Body.String(); got != "[]\n" {
			t.Fatalf("%s: expected empty-array body \"[]\", got %q", path, got)
		}
	}
}
