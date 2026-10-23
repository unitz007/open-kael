package api_test

import (
	"bytes"
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

// The browser reports the person's own time zone and it is kept, but only when they have none: a
// zone they chose is never overwritten by a guess, and nonsense is refused.
func TestTimezoneHint(t *testing.T) {
	ctx := context.Background()
	store, _ := pgtest.Store(t)
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
