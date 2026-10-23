package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// A person can ask to be asked first about something that changes something, never about a lookup.
func TestMyToolApproval_OnlyForThingsThatChangeSomething(t *testing.T) {
	ctx := context.Background()
	store, _ := pgtest.Store(t)
	srv := api.NewServer(store)

	const user = "mta_a"
	if _, err := seedLogin(ctx, store, user, user+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + user, AccountID: user, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "mta_int", Service: "mta", Name: "Mta"}); err != nil {
		t.Fatal(err)
	}
	look := &domain.ToolDefinition{ID: "mta_look", Name: "look", Description: "Looks", IntegrationID: "mta_int", Action: "a", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	change := &domain.ToolDefinition{ID: "mta_change", Name: "change", Description: "Changes", IntegrationID: "mta_int", Action: "b", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	for _, td := range []*domain.ToolDefinition{look, change} {
		if err := store.SaveTool(ctx, td); err != nil {
			t.Fatal(err)
		}
	}
	put := func(tool string) (int, string) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/users/me/tool-approvals/"+tool, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer tok_"+user)
		srv.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}
	if code, body := put("mta_look"); code != http.StatusBadRequest || !strings.Contains(body, "only looks things up") {
		t.Errorf("a lookup is refused with a reason: %d %s", code, body)
	}
	if code, body := put("mta_change"); code != http.StatusOK {
		t.Errorf("a change is accepted: %d %s", code, body)
	}
}
