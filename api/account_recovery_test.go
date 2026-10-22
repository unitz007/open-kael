package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/api"
)

// inbox is a mailer that keeps what it sent.
type inbox struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ to, subject, html string }

func (b *inbox) Send(_ context.Context, to, subject, html string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, sentMail{to, subject, html})
	return nil
}

func (b *inbox) take() []sentMail {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.sent
	b.sent = nil
	return out
}

var linkRE = regexp.MustCompile(`https://app\.test/(verify|reset)\?token=([0-9a-f]+)`)

func linkIn(t *testing.T, m sentMail, kind string) string {
	t.Helper()
	for _, l := range linkRE.FindAllStringSubmatch(m.html, -1) {
		if l[1] == kind {
			return l[2]
		}
	}
	t.Fatalf("no %s link in %q", kind, m.subject)
	return ""
}

func mailRig(t *testing.T) (*api.Server, *inbox) {
	t.Helper()
	store, _ := openDB(t)
	box := &inbox{}
	return api.NewServer(store, api.WithMailer(box, "https://app.test")), box
}

func verify(t *testing.T, srv *api.Server, token string) {
	t.Helper()
	req := httpGet("/users/verify?token=" + token)
	rec := serve(srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
}

// Registering always answers "check your inbox", whether or not the address has a login, so it never tells anyone
// which addresses do. A new address gets the link to confirm it; a confirmed login's owner is told someone tried; a
// login that was never confirmed is not trusted, so it takes the new password and the link is sent again.
func TestRegister_TheSameAnswerForEveryAddressAndTheOwnerIsToldByEmail(t *testing.T) {
	srv, box := mailRig(t)
	reg := map[string]string{"email": "grace@example.com", "password": "first password", "first_name": "Grace"} // no last name needed

	first := postJSON(t, srv, "/users", reg)
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"check_inbox":true`) {
		t.Fatalf("a new address: %d %s", first.Code, first.Body.String())
	}
	mails := box.take()
	if len(mails) != 1 || mails[0].to != "grace@example.com" {
		t.Fatalf("the link to confirm it: %+v", mails)
	}
	firstLink := linkIn(t, mails[0], "verify")

	// Someone registers the same, still unconfirmed, address: the same answer, their password, a fresh link.
	again := map[string]string{"email": "Grace@Example.com", "password": "second password", "first_name": "Mallory"}
	if rec := postJSON(t, srv, "/users", again); rec.Code != first.Code || rec.Body.String() != first.Body.String() {
		t.Errorf("the same answer as for a new address: %d %s", rec.Code, rec.Body.String())
	}
	mails = box.take()
	if len(mails) != 1 {
		t.Fatalf("the link again: %+v", mails)
	}
	link := linkIn(t, mails[0], "verify")
	if link == firstLink {
		t.Error("an unconfirmed login gets a fresh link")
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "grace@example.com", "password": "second password"}); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Confirm your email") {
		t.Errorf("not confirmed yet, said plainly: %d %s", rec.Code, rec.Body.String())
	}
	verify(t, srv, link)
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "grace@example.com", "password": "second password"}); rec.Code != http.StatusOK {
		t.Errorf("whoever confirmed the address signs in with their password: %d %s", rec.Code, rec.Body.String())
	}

	// Now it is confirmed: registering it again changes nothing, and its owner is told.
	if rec := postJSON(t, srv, "/users", map[string]string{"email": "grace@example.com", "password": "third password", "first_name": "Eve"}); rec.Code != first.Code {
		t.Errorf("still the same answer: %d", rec.Code)
	}
	mails = box.take()
	if len(mails) != 1 || mails[0].subject != "You already have an account" {
		t.Fatalf("the owner is told: %+v", mails)
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "grace@example.com", "password": "third password"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("a confirmed login's password never changes by registering: %d", rec.Code)
	}
}

func TestRegister_PasswordsHaveAMinimumAndErrorsArePlain(t *testing.T) {
	srv, _ := mailRig(t)
	for body, want := range map[string]string{
		`{"email":"a@example.com","password":"short","first_name":"A"}`:      "at least 8 characters",
		`{"email":"a@example.com","password":"long enough","first_name":""}`: "Fill in your name",
		`{"email":"not an email","password":"long enough","first_name":"A"}`: "doesn’t look like an email",
	} {
		rec := postRaw(srv, "/users", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

// The link to confirm an address can be sent again, only to a login that has not confirmed it; the answer is the same
// for any address.
func TestResendVerification_OnlyToAnUnconfirmedLoginWithTheSameAnswer(t *testing.T) {
	srv, box := mailRig(t)
	postJSON(t, srv, "/users", map[string]string{"email": "ada@example.com", "password": "long enough", "first_name": "Ada"})
	link := linkIn(t, box.take()[0], "verify")

	if rec := postJSON(t, srv, "/users/verify/resend", map[string]string{"email": "ADA@example.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("resend: %d", rec.Code)
	}
	if mails := box.take(); len(mails) != 1 || linkIn(t, mails[0], "verify") != link {
		t.Errorf("the same link again: %+v", mails)
	}
	if rec := postJSON(t, srv, "/users/verify/resend", map[string]string{"email": "nobody@example.com"}); rec.Code != http.StatusAccepted || len(box.take()) != 0 {
		t.Errorf("an unknown address: the same answer, nothing sent: %d", rec.Code)
	}
	verify(t, srv, link)
	if postJSON(t, srv, "/users/verify/resend", map[string]string{"email": "ada@example.com"}); len(box.take()) != 0 {
		t.Error("a confirmed login is not sent it again")
	}
	// At most three emails of these kinds an hour to one address.
	postJSON(t, srv, "/users", map[string]string{"email": "bea@example.com", "password": "long enough", "first_name": "Bea"})
	for i := 0; i < 4; i++ {
		postJSON(t, srv, "/users/verify/resend", map[string]string{"email": "bea@example.com"})
	}
	if n := len(box.take()); n != 3 {
		t.Errorf("three an hour, the registration's included: %d", n)
	}
}

// Forgot password: a one-time link, good for an hour; using it sets the new password, confirms the address and signs
// the login out everywhere else. An unknown address gets the same answer and no email.
func TestPasswordReset_AOneTimeLinkSetsANewPasswordAndSignsOutEverywhere(t *testing.T) {
	srv, box := mailRig(t)
	postJSON(t, srv, "/users", map[string]string{"email": "joan@example.com", "password": "old password", "first_name": "Joan"})
	verify(t, srv, linkIn(t, box.take()[0], "verify"))
	login := postJSON(t, srv, "/sessions", map[string]string{"email": "joan@example.com", "password": "old password"})
	var sess struct{ Token string }
	_ = json.Unmarshal(login.Body.Bytes(), &sess)

	if rec := postJSON(t, srv, "/users/password/forgot", map[string]string{"email": "nobody@example.com"}); rec.Code != http.StatusAccepted || len(box.take()) != 0 {
		t.Fatalf("an unknown address: the same answer, nothing sent: %d", rec.Code)
	}
	if rec := postJSON(t, srv, "/users/password/forgot", map[string]string{"email": "Joan@example.com"}); rec.Code != http.StatusAccepted {
		t.Fatalf("forgot: %d", rec.Code)
	}
	mails := box.take()
	if len(mails) != 1 || mails[0].subject != "Choose a new password" {
		t.Fatalf("a link: %+v", mails)
	}
	token := linkIn(t, mails[0], "reset")

	if rec := postJSON(t, srv, "/users/password/reset", map[string]string{"token": token, "password": "short"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least 8") {
		t.Errorf("too short, and the link is not used up by it: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, srv, "/users/password/reset", map[string]string{"token": token, "password": "new password"}); rec.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, srv, "/users/password/reset", map[string]string{"token": token, "password": "another one"}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "already used") {
		t.Errorf("a link works once: %d %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "joan@example.com", "password": "old password"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the old password no longer works: %d", rec.Code)
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "joan@example.com", "password": "new password"}); rec.Code != http.StatusOK {
		t.Errorf("the new one does: %d", rec.Code)
	}
	req := httpGet("/users/me")
	req.Header.Set("Authorization", "Bearer "+sess.Token)
	if rec := serve(srv, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out everywhere else: %d", rec.Code)
	}
	if rec := postJSON(t, srv, "/users/password/reset", map[string]string{"token": "0123abcd", "password": "new password"}); rec.Code != http.StatusBadRequest {
		t.Errorf("a made-up link: %d", rec.Code)
	}
}

// Guessing is slow: after ten wrong passwords for an address, it is refused for a while, even with the right one.
func TestSignIn_TooManyWrongPasswordsAreRefusedForAWhile(t *testing.T) {
	srv, box := mailRig(t)
	postJSON(t, srv, "/users", map[string]string{"email": "kim@example.com", "password": "right password", "first_name": "Kim"})
	verify(t, srv, linkIn(t, box.take()[0], "verify"))

	for i := 0; i < 9; i++ {
		postJSON(t, srv, "/sessions", map[string]string{"email": "kim@example.com", "password": "wrong"})
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "kim@example.com", "password": "right password"}); rec.Code != http.StatusOK {
		t.Fatalf("nine wrong tries still let the right one in: %d", rec.Code)
	}
	for i := 0; i < 10; i++ { // the success started the count again
		if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "kim@example.com", "password": "wrong"}); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "don’t match") {
			t.Fatalf("a wrong password, said plainly: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := postJSON(t, srv, "/sessions", map[string]string{"email": "kim@example.com", "password": "right password"}); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "Too many tries") {
		t.Errorf("the eleventh is refused, right or not: %d %s", rec.Code, rec.Body.String())
	}
}

func httpGet(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

func serve(srv *api.Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func postRaw(srv *api.Server, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return serve(srv, req)
}
