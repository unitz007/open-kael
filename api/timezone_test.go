package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// The browser reports the person's own time zone and it is kept, but only when they have none: a
// zone they chose is never overwritten by a guess, and nonsense is refused.
func TestTimezoneHint(t *testing.T) {
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
	srv := api.NewServer(store)

	users := []string{"tzh_a", "tzh_b"}
	for _, u := range users {
		if _, err := seedLogin(ctx, store, u, u+"@example.com"); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, AccountID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, u := range users {
			_ = store.DeleteSession(ctx, "tok_"+u)
			_ = store.DeleteAccount(ctx, u)
			_ = store.DeletePerson(ctx, u)
		}
	})

	post := func(token, zone string) (int, map[string]any) {
		raw, _ := json.Marshal(map[string]string{"timezone": zone})
		req := httptest.NewRequest(http.MethodPost, "/users/me/timezone-hint", bytes.NewReader(raw))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, out := post("tok_tzh_a", "Africa/Lagos"); code != 200 || out["saved"] != true || out["timezone"] != "Africa/Lagos" {
		t.Fatalf("first report is kept: %d %v", code, out)
	}
	if code, out := post("tok_tzh_a", "Europe/Berlin"); code != 200 || out["saved"] != false || out["timezone"] != "Africa/Lagos" {
		t.Errorf("a later report never overwrites: %d %v", code, out)
	}
	if tz, _ := store.UserTimezone(ctx, "tzh_a"); tz != "Africa/Lagos" {
		t.Errorf("stored: %q", tz)
	}
	if tz, _ := store.UserTimezone(ctx, "tzh_b"); tz != "" {
		t.Errorf("another person's zone is untouched: %q", tz)
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "not a zone", strings.Repeat("A", 100), "UTC+1"} {
		if code, _ := post("tok_tzh_b", bad); code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if tz, _ := store.UserTimezone(ctx, "tzh_b"); tz != "" {
		t.Errorf("nonsense is never kept: %q", tz)
	}
	if code, _ := post("", "Africa/Lagos"); code != http.StatusUnauthorized {
		t.Errorf("no sign-in: %d", code)
	}
}
