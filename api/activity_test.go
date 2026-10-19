package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// A creator sees how their own agents are doing, and only those: not someone else's, not the
// platform's. The answer is counts, never anyone's words or identity.
func TestActivity_ACreatorSeesOnlyTheirOwnAgents(t *testing.T) {
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
	srv := api.NewServer(store, api.WithActivity(store))

	users := []string{"act_a", "act_b"}
	for _, u := range users {
		if err := store.SaveAccount(ctx, &domain.Account{ID: u, Email: u + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, AccountID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `TRUNCATE agents CASCADE`)
		for _, u := range users {
			_ = store.DeleteSession(ctx, "tok_"+u)
			_ = store.DeleteAccount(ctx, u)
		}
	})
	for _, a := range []*domain.Agent{
		{ID: "act_ag_a", Name: "A's", CreatedBy: "act_a", MaxIterations: 5},
		{ID: "act_ag_b", Name: "B's", CreatedBy: "act_b", MaxIterations: 5},
		{ID: "act_ag_plat", Name: "Platform's", MaxIterations: 5},
	} {
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	store.PersonActive(ctx, "act_ag_a", "x1")
	store.PersonActive(ctx, "act_ag_a", "x2")
	store.PersonActive(ctx, "act_ag_b", "y1")
	store.PersonActive(ctx, "act_ag_plat", "z1")
	store.ActivityEvent(ctx, "act_ag_b", domain.ActivityFailed, "problem")

	get := func(token, path string) (int, domain.ActivitySummary) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var sum domain.ActivitySummary
		_ = json.Unmarshal(rec.Body.Bytes(), &sum)
		return rec.Code, sum
	}

	if code, sum := get("tok_act_a", "/activity?days=7"); code != 200 || sum.ActiveToday != 2 || len(sum.Daily) != 7 || sum.FailedToday != 0 {
		t.Errorf("all of A's agents are A's alone (not B's, not the platform's): %d %+v", code, sum)
	}
	if code, sum := get("tok_act_a", "/activity?agent_id=act_ag_a"); code != 200 || sum.ActiveToday != 2 || len(sum.Daily) != 14 {
		t.Errorf("one of A's agents, 14 days by default: %d %+v", code, sum)
	}
	for _, path := range []string{"/activity?agent_id=act_ag_b", "/activity?agent_id=act_ag_plat", "/activity?agent_id=nope"} {
		if code, _ := get("tok_act_a", path); code != http.StatusNotFound {
			t.Errorf("%s must not be A's to see: %d", path, code)
		}
	}
	if code, sum := get("tok_act_b", "/activity"); code != 200 || sum.ActiveToday != 1 || sum.FailedToday != 1 {
		t.Errorf("B sees B's: %d %+v", code, sum)
	}
	for _, bad := range []string{"/activity?days=0", "/activity?days=31", "/activity?days=x"} {
		if code, _ := get("tok_act_a", bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ := get("", "/activity"); code != http.StatusUnauthorized {
		t.Errorf("no sign-in: %d", code)
	}
	// A server-to-server caller with the platform key sees everything; with no key set, nobody
	// without a sign-in sees anything.
	keyed := api.NewServer(store, api.WithActivity(store), api.WithBearerAuth("secret"))
	rec0 := httptest.NewRecorder()
	r0 := httptest.NewRequest(http.MethodGet, "/activity", nil)
	r0.Header.Set("Authorization", "Bearer secret")
	keyed.ServeHTTP(rec0, r0)
	var all domain.ActivitySummary
	_ = json.Unmarshal(rec0.Body.Bytes(), &all)
	if rec0.Code != 200 || all.ActiveToday != 4 {
		t.Errorf("the platform key sees every agent: %d %+v", rec0.Code, all)
	}

	// Nothing in the answer is a name, a message or an address.
	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	req.Header.Set("Authorization", "Bearer tok_act_a")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for k := range raw {
		switch k {
		case "connected", "connected_this_week", "active_today", "active_week", "failed_today", "daily", "chat_apps", "topics", "problems", "events", "recent_messages", "recent_window_minutes":
		default:
			t.Errorf("unexpected field in the activity answer: %q", k)
		}
	}
	none := api.NewServer(store)
	rec = httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/activity", nil)
	r2.Header.Set("Authorization", "Bearer tok_act_a")
	none.ServeHTTP(rec, r2)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("not set up: %d", rec.Code)
	}
}
