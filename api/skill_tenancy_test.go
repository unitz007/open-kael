package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// A creator must not be able to run another creator's private connection (and
// so its key) by naming it in a skill.
func TestSkillCannotUseSomeoneElsesConnection(t *testing.T) {
	ctx := context.Background()
	store, _ := pgtest.Store(t)
	srv := api.NewServer(store)

	for _, u := range []string{"ten_a", "ten_b"} {
		if err := store.SaveAccount(ctx, &domain.Account{ID: u, Email: u + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, AccountID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "ten_plat", Name: "Platform", Service: "plat"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "ten_priv", Name: "B's", Service: "ten_priv", CreatedBy: "ten_b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "ten_priv_tool", Name: "t", IntegrationID: "ten_priv"}); err != nil {
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

	newAgent := func(token, name string) string {
		rec := call(token, http.MethodPost, "/agents", map[string]any{"name": name})
		var a domain.Agent
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &a) != nil || a.ID == "" {
			t.Fatalf("create agent: %d %s", rec.Code, rec.Body.String())
		}
		return a.ID
	}
	agentA := newAgent("tok_ten_a", "A")

	skill := func(id string, integrations []string, tools ...string) map[string]any {
		var bindings []map[string]string
		for _, tl := range tools {
			bindings = append(bindings, map[string]string{"tool_id": tl})
		}
		return map[string]any{"id": id, "name": id, "integrations": integrations, "tools": bindings}
	}

	for name, body := range map[string]map[string]any{
		"integration grant": skill("ten_s1", []string{"ten_priv"}),
		"tool binding":      skill("ten_s2", nil, "ten_priv_tool"),
		"unknown":           skill("ten_s3", []string{"nope"}),
	} {
		if rec := call("tok_ten_a", http.MethodPost, "/agents/"+agentA+"/skills", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := call("tok_ten_a", http.MethodPost, "/agents/"+agentA+"/skills", skill("ten_s4", []string{"ten_plat"})); rec.Code != http.StatusOK {
		t.Errorf("platform integration should be allowed: %d %s", rec.Code, rec.Body.String())
	}
	// Editing is guarded the same way.
	if rec := call("tok_ten_a", http.MethodPut, "/skills/ten_s4", skill("ten_s4", []string{"ten_priv"})); rec.Code != http.StatusBadRequest {
		t.Errorf("update to someone else's connection: want 400, got %d %s", rec.Code, rec.Body.String())
	}
	// B can use their own.
	agentB := newAgent("tok_ten_b", "B")
	if rec := call("tok_ten_b", http.MethodPost, "/agents/"+agentB+"/skills", skill("ten_s5", []string{"ten_priv"})); rec.Code != http.StatusOK {
		t.Errorf("owner should be allowed: %d %s", rec.Code, rec.Body.String())
	}
}
