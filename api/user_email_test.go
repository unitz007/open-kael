package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unitz007/open-kael/api"
)

func postJSON(t *testing.T, srv *api.Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// However a person types their email, it is the same login: they can sign in with a different case
// from the one they registered with, and cannot register the same address twice by changing it.
func TestRegisterAndLogin_EmailCaseDoesNotMatter(t *testing.T) {
	store := openDB(t)
	srv := api.NewServer(store)
	reg := map[string]string{"email": "  Ada.Lovelace@Example.COM ", "password": "correct horse", "first_name": "Ada", "last_name": "Lovelace"}

	rec := postJSON(t, srv, "/users", reg)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	var made struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if made.Email != "ada.lovelace@example.com" {
		t.Errorf("stored lower case and trimmed: %q", made.Email)
	}

	for _, typed := range []string{"ada.lovelace@example.com", "ADA.LOVELACE@EXAMPLE.COM", " Ada.Lovelace@Example.com "} {
		if rec := postJSON(t, srv, "/sessions", map[string]string{"email": typed, "password": "correct horse"}); rec.Code != http.StatusOK {
			t.Errorf("signing in as %q: %d %s", typed, rec.Code, rec.Body.String())
		}
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "ADA.LOVELACE@example.com", "password": "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the case is forgiven, the password is not: %d", rec.Code)
	}

	again := map[string]string{"email": "ADA.LOVELACE@example.com", "password": "another one", "first_name": "Eve", "last_name": "Other"}
	if rec := postJSON(t, srv, "/users", again); rec.Code != http.StatusConflict {
		t.Errorf("the same address in another case is already taken: %d %s", rec.Code, rec.Body.String())
	}
}
