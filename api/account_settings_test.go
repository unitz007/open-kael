package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

var changeRE = regexp.MustCompile(`https://app\.test/change-email\?token=([0-9a-f]+)`)

// signedUp registers and confirms a login and signs it in, returning its session token.
func signedUp(t *testing.T, srv *api.Server, box *inbox, email, password string) string {
	t.Helper()
	postJSON(t, srv, "/users", map[string]string{"email": email, "password": password, "first_name": "Ada", "last_name": "Lovelace"})
	verify(t, srv, linkIn(t, box.take()[0], "verify"))
	return signIn(t, srv, email, password)
}

func signIn(t *testing.T, srv *api.Server, email, password string) string {
	t.Helper()
	rec := postJSON(t, srv, "/sessions", map[string]string{"email": email, "password": password})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in %s: %d %s", email, rec.Code, rec.Body.String())
	}
	var s struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &s)
	return s.Token
}

func as(t *testing.T, srv *api.Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return serve(srv, req)
}

func accountRig(t *testing.T, opts ...api.Option) (*api.Server, *inbox, *postgres.Store) {
	t.Helper()
	store := openDB(t)
	box := &inbox{}
	return api.NewServer(store, append([]api.Option{api.WithMailer(box, "https://app.test")}, opts...)...), box, store
}

// The account page's facts, and a new name (one word is a whole name).
func TestAccount_ShowsTheLoginAndRenames(t *testing.T) {
	srv, box, _ := accountRig(t)
	here := signedUp(t, srv, box, "ada@example.com", "first password")
	signIn(t, srv, "ada@example.com", "first password")

	rec := as(t, srv, here, http.MethodGet, "/users/me/account", nil)
	var got struct {
		Email          string `json:"email"`
		FirstName      string `json:"first_name"`
		HasPassword    bool   `json:"has_password"`
		SignedInOn     int    `json:"signed_in_on"`
		CanChangeEmail bool   `json:"can_change_email"`
		CreatedAt      string `json:"created_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Email != "ada@example.com" || got.FirstName != "Ada" || !got.HasPassword || got.SignedInOn != 2 || !got.CanChangeEmail || got.CreatedAt == "" {
		t.Fatalf("account: %d %s", rec.Code, rec.Body.String())
	}

	if rec := as(t, srv, here, http.MethodPatch, "/users/me", map[string]string{"first_name": "  "}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Fill in your name") {
		t.Errorf("an empty name: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, here, http.MethodPatch, "/users/me", map[string]string{"first_name": " Augusta ", "last_name": ""}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"first_name":"Augusta"`) || strings.Contains(rec.Body.String(), "Lovelace") {
		t.Errorf("renamed, the surname dropped: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, here, http.MethodGet, "/users/me", nil); !strings.Contains(rec.Body.String(), `"first_name":"Augusta"`) {
		t.Errorf("kept: %s", rec.Body.String())
	}
}

// A new password needs the current one, and changing it signs the login out everywhere but here; so does "sign out
// the others" on its own.
func TestAccount_PasswordAndSigningOutElsewhere(t *testing.T) {
	srv, box, _ := accountRig(t)
	here := signedUp(t, srv, box, "grace@example.com", "first password")
	there := signIn(t, srv, "grace@example.com", "first password")

	if rec := as(t, srv, here, http.MethodPost, "/users/me/password", map[string]string{"current": "wrong", "password": "second password"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "current password") {
		t.Errorf("the wrong current password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, here, http.MethodPost, "/users/me/password", map[string]string{"current": "first password", "password": "short"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least 8") {
		t.Errorf("too short: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, here, http.MethodPost, "/users/me/password", map[string]string{"current": "first password", "password": "second password"}); rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, here, http.MethodGet, "/users/me", nil); rec.Code != http.StatusOK {
		t.Errorf("still signed in here: %d", rec.Code)
	}
	if rec := as(t, srv, there, http.MethodGet, "/users/me", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out elsewhere: %d", rec.Code)
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "grace@example.com", "password": "first password"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password no longer works: %d", rec.Code)
	}

	there = signIn(t, srv, "grace@example.com", "second password")
	if rec := as(t, srv, here, http.MethodPost, "/users/me/sessions/sign-out-others", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out the others: %d %s", rec.Code, rec.Body.String())
	}
	if as(t, srv, there, http.MethodGet, "/users/me", nil).Code != http.StatusUnauthorized || as(t, srv, here, http.MethodGet, "/users/me", nil).Code != http.StatusOK {
		t.Error("only the others are signed out")
	}
}

// A new email changes only when the link sent to it is confirmed (opening it only shows what it would do); the old
// address is told. An address that has a login of its own gets a note instead, and the answer is the same.
func TestAccount_EmailChangesOnlyOnceTheLinkIsConfirmed(t *testing.T) {
	srv, box, _ := accountRig(t)
	here := signedUp(t, srv, box, "joan@example.com", "long enough")
	signedUp(t, srv, box, "taken@example.com", "long enough")

	if rec := as(t, srv, here, http.MethodPost, "/users/me/email", map[string]string{"email": "JOAN@example.com"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already your email") {
		t.Errorf("the same address: %d %s", rec.Code, rec.Body.String())
	}
	taken := as(t, srv, here, http.MethodPost, "/users/me/email", map[string]string{"email": "taken@example.com"})
	mails := box.take()
	if taken.Code != http.StatusAccepted || len(mails) != 1 || mails[0].subject != "This email already has a login" || changeRE.MatchString(mails[0].html) {
		t.Fatalf("an address with its own login gets a note, no link: %d %+v", taken.Code, mails)
	}

	rec := as(t, srv, here, http.MethodPost, "/users/me/email", map[string]string{"email": "Joan.New@example.com"})
	mails = box.take()
	if rec.Code != http.StatusAccepted || len(mails) != 1 || mails[0].to != "joan.new@example.com" {
		t.Fatalf("a link to the new address: %d %+v", rec.Code, mails)
	}
	token := changeRE.FindStringSubmatch(mails[0].html)[1]

	peek := serve(srv, httpGet("/users/email/change?token="+token))
	if peek.Code != http.StatusOK || !strings.Contains(peek.Body.String(), `"email":"joan.new@example.com"`) || !strings.Contains(peek.Body.String(), `"from":"joan@example.com"`) {
		t.Fatalf("what the link would do: %d %s", peek.Code, peek.Body.String())
	}
	if rec := as(t, srv, here, http.MethodGet, "/users/me", nil); !strings.Contains(rec.Body.String(), `"email":"joan@example.com"`) {
		t.Errorf("opening the link changes nothing: %s", rec.Body.String())
	}
	if rec := postJSON(t, srv, "/users/email/change", map[string]string{"token": token}); rec.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	mails = box.take()
	if len(mails) != 1 || mails[0].to != "joan@example.com" || !strings.Contains(mails[0].html, "joan.new@example.com") {
		t.Errorf("the old address is told: %+v", mails)
	}
	if rec := postJSON(t, srv, "/users/email/change", map[string]string{"token": token}); rec.Code != http.StatusBadRequest {
		t.Errorf("a link works once: %d", rec.Code)
	}
	signIn(t, srv, "joan.new@example.com", "long enough")
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "joan@example.com", "password": "long enough"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old address no longer signs in: %d", rec.Code)
	}
}

// Deleting a login (confirmed by its email) deletes what its creator built and nobody else's: their agents, the hook's
// own things, their accounts on services and their own connections, then the login.
func TestAccount_DeleteTakesOnlyWhatTheCreatorBuilt(t *testing.T) {
	var hooked []string
	srv, box, store := accountRig(t, api.WithAccountDeleteHook(func(_ context.Context, id string) error { hooked = append(hooked, id); return nil }))
	mine := signedUp(t, srv, box, "kim@example.com", "long enough")
	theirs := signedUp(t, srv, box, "lee@example.com", "long enough")
	ctx := context.Background()
	idOf := func(token string) string {
		var m struct{ ID string }
		json.Unmarshal(as(t, srv, token, http.MethodGet, "/users/me", nil).Body.Bytes(), &m)
		return m.ID
	}
	kim, lee := idOf(mine), idOf(theirs)

	for _, in := range []*domain.Integration{{ID: "conn-kim", Name: "Kim's", Service: "conn-kim", CreatedBy: kim}, {ID: "conn-lee", Name: "Lee's", Service: "conn-lee", CreatedBy: lee}} {
		if err := store.SaveIntegration(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []*domain.Identity{{ID: "bot-kim", IntegrationID: "conn-kim", Name: "bot", Kind: "bot", CreatedBy: kim}, {ID: "bot-lee", IntegrationID: "conn-lee", Name: "bot", Kind: "bot", CreatedBy: lee}} {
		if err := store.SaveIdentity(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []*domain.Agent{{ID: "agent-kim", Name: "Kim's", CreatedBy: kim, IdentityIDs: []string{"bot-kim"}}, {ID: "agent-lee", Name: "Lee's", CreatedBy: lee}} {
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	if rec := as(t, srv, mine, http.MethodDelete, "/users/me", map[string]string{"email": "someone@example.com"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Type your email") {
		t.Fatalf("confirmed by its own email: %d %s", rec.Code, rec.Body.String())
	}
	if rec := as(t, srv, mine, http.MethodDelete, "/users/me", map[string]string{"email": " KIM@example.com "}); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if len(hooked) != 1 || hooked[0] != kim {
		t.Errorf("the hook ran for the login: %v", hooked)
	}
	if _, err := store.LoadAgent(ctx, "agent-kim"); err == nil {
		t.Error("their agent is gone")
	}
	if _, err := store.GetIdentity(ctx, "bot-kim"); err == nil {
		t.Error("their bot is gone")
	}
	if _, err := store.LoadIntegration(ctx, "conn-kim"); err == nil {
		t.Error("their connection is gone")
	}
	if _, err := store.GetAccount(ctx, kim); err == nil {
		t.Error("the login is gone")
	}
	if as(t, srv, mine, http.MethodGet, "/users/me", nil).Code != http.StatusUnauthorized {
		t.Error("signed out")
	}
	if _, err := store.LoadAgent(ctx, "agent-lee"); err != nil {
		t.Error("someone else's agent stays")
	}
	if _, err := store.GetIdentity(ctx, "bot-lee"); err != nil {
		t.Error("someone else's bot stays")
	}
	if _, err := store.LoadIntegration(ctx, "conn-lee"); err != nil {
		t.Error("someone else's connection stays")
	}
	if postJSON(t, srv, "/sessions", map[string]string{"email": "kim@example.com", "password": "long enough"}).Code != http.StatusUnauthorized {
		t.Error("the address no longer signs in")
	}
}
