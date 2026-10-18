package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// Rules on how an agent uses a tool: set by the agent's creator only, checked against the tool,
// limited to tools that creator may see, and applied by reloading the agent.
func TestAgentRules(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := postgres.New(pool)

	var mu sync.Mutex
	var reloaded []string
	srv := api.NewServer(store, api.WithAgentUpdateHook(func(id string) {
		mu.Lock()
		reloaded = append(reloaded, id)
		mu.Unlock()
	}))

	users := []string{"rul_a", "rul_b"}
	for _, u := range users {
		if err := store.SaveUser(ctx, &domain.User{ID: u, Email: u + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, UserID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `TRUNCATE skill_tools, skills, tools, agents, integrations CASCADE`)
		for _, u := range users {
			_ = store.DeleteSession(ctx, "tok_"+u)
			_ = store.DeleteUser(ctx, u)
		}
	})
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "rul_plat", Name: "Platform", Service: "plat"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "rul_priv", Name: "B's", Service: "rul_priv", CreatedBy: "rul_b"}); err != nil {
		t.Fatal(err)
	}
	trade := &domain.ToolDefinition{ID: "rul_trade", Name: "place_trade", IntegrationID: "rul_plat", InputSchema: domain.Schema{
		Type: domain.SchemaTypeObject,
		Properties: map[string]domain.Schema{
			"symbol": {Type: domain.SchemaTypeString},
			"size":   {Type: domain.SchemaTypeInteger},
		},
	}}
	if err := store.SaveTool(ctx, trade); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "rul_priv_tool", Name: "t", IntegrationID: "rul_priv", InputSchema: domain.Schema{
		Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"x": {Type: domain.SchemaTypeInteger}},
	}}); err != nil {
		t.Fatal(err)
	}

	call := func(token, method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	rec := call("tok_rul_a", http.MethodPost, "/agents", map[string]any{"name": "Desk"})
	var agent domain.Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil || agent.ID == "" {
		t.Fatalf("create agent: %d %s", rec.Code, rec.Body)
	}
	path := "/agents/" + agent.ID + "/rules"

	// A valid set is saved, made canonical ("100000" becomes a number), given IDs, and applied by a reload.
	rec = call("tok_rul_a", http.MethodPut, path, []map[string]any{
		{"tool_id": "rul_trade", "input": "size", "kind": "max", "value": "100000", "message": "Too big for me."},
		{"tool_id": "rul_trade", "input": "symbol", "kind": "one_of", "value": []string{"EURUSD", "GBPUSD"}},
	})
	if rec.Code != 200 {
		t.Fatalf("set rules: %d %s", rec.Code, rec.Body)
	}
	rec = call("tok_rul_a", http.MethodGet, path, nil)
	var got []domain.ToolRule
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 2 {
		t.Fatalf("get rules: %d %s", rec.Code, rec.Body)
	}
	if got[0].Value != 100000.0 || got[0].ID == "" || got[0].Message != "Too big for me." {
		t.Errorf("rules are saved in canonical form with IDs: %+v", got[0])
	}
	mu.Lock()
	if len(reloaded) != 1 || reloaded[0] != agent.ID {
		t.Errorf("saving reloads the agent so the rules apply: %v", reloaded)
	}
	mu.Unlock()
	if loaded, _ := store.LoadAgent(ctx, agent.ID); len(loaded.Rules) != 2 {
		t.Errorf("what the runtime loads has the rules: %+v", loaded.Rules)
	}

	// A rule that can't work is refused in plain words, and the saved rules stay as they were.
	for name, body := range map[string][]map[string]any{
		"an input the tool doesn't take": {{"tool_id": "rul_trade", "input": "nope", "kind": "max", "value": 1}},
		"a limit on text":                {{"tool_id": "rul_trade", "input": "symbol", "kind": "max", "value": 1}},
		"rules that can't both hold":     {{"tool_id": "rul_trade", "input": "size", "kind": "min", "value": 20}, {"tool_id": "rul_trade", "input": "size", "kind": "max", "value": 10}},
		"a tool that doesn't exist":      {{"tool_id": "ghost", "input": "size", "kind": "max", "value": 1}},
		"another creator's connection":   {{"tool_id": "rul_priv_tool", "input": "x", "kind": "max", "value": 1}},
	} {
		rec = call("tok_rul_a", http.MethodPut, path, body)
		if rec.Code != 400 || strings.Contains(rec.Body.String(), "rul_") {
			t.Errorf("%s: want 400 without internal names, got %d %s", name, rec.Code, rec.Body)
		}
	}
	if after, _ := store.GetAgent(ctx, agent.ID); len(after.Rules) != 2 {
		t.Errorf("a refused save leaves the rules alone: %+v", after.Rules)
	}

	// Another creator can neither read nor change them.
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if rec = call("tok_rul_b", method, path, []map[string]any{}); rec.Code != 404 {
			t.Errorf("%s by another creator: want 404, got %d", method, rec.Code)
		}
	}
	if after, _ := store.GetAgent(ctx, agent.ID); len(after.Rules) != 2 {
		t.Errorf("the other creator changed nothing: %+v", after.Rules)
	}

	// Saving an empty list clears them.
	if rec = call("tok_rul_a", http.MethodPut, path, []map[string]any{}); rec.Code != 200 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if after, _ := store.GetAgent(ctx, agent.ID); len(after.Rules) != 0 {
		t.Errorf("cleared: %+v", after.Rules)
	}

	// The same call works for a creator on their own private connection.
	rec = call("tok_rul_b", http.MethodPost, "/agents", map[string]any{"name": "B's"})
	var bAgent domain.Agent
	_ = json.Unmarshal(rec.Body.Bytes(), &bAgent)
	if rec = call("tok_rul_b", http.MethodPut, "/agents/"+bAgent.ID+"/rules", []map[string]any{{"tool_id": "rul_priv_tool", "input": "x", "kind": "max", "value": 5}}); rec.Code != 200 {
		t.Errorf("a creator may set rules on their own connection: %d %s", rec.Code, rec.Body)
	}
}
