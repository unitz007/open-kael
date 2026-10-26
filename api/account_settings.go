package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/unitz007/open-kael/domain"
)

// A signed-in creator managing their own login: their name, password, where they are signed in, moving it to a new
// email, and deleting it with everything they built. A new email is used only once the link sent to it is opened and
// confirmed (mail scanners open links, so opening one never changes anything).

var (
	errEmailLink      = errors.New("This link has run out or was already used. Ask for a new one from your account page.")
	errWrongPassword  = errors.New("That isn’t your current password.")
	errEmailTaken     = errors.New("That email now has its own login, so yours can’t move to it.")
	errNotAvailable   = errors.New("This isn’t available here.")
	errConfirmByEmail = errors.New("Type your email exactly as it is to confirm.")
)

// sessionToken is the session the request was made with (userAuth has checked it).
func sessionToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

type myAccount struct {
	me
	CreatedAt   time.Time `json:"created_at"`
	HasPassword bool      `json:"has_password"`
	// SignedInOn is how many places the login is signed in on now, this one included.
	SignedInOn int `json:"signed_in_on"`
	// CanChangeEmail is false where no email can be sent.
	CanChangeEmail bool `json:"can_change_email"`
}

func (s *Server) getMyAccount(w http.ResponseWriter, r *http.Request) {
	account, _ := AccountFromContext(r.Context())
	person, _ := PersonFromContext(r.Context())
	out := myAccount{me: meResponse(account, person), CreatedAt: account.CreatedAt, HasPassword: account.PasswordHash != "", SignedInOn: 1, CanChangeEmail: s.mailer != nil}
	if st, ok := s.store.(domain.AccountSettingsStore); ok {
		if n, err := st.CountSessionsOf(r.Context(), account.ID, time.Now()); err == nil && n > 0 {
			out.SignedInOn = n
		}
	} else {
		out.CanChangeEmail = false
	}
	writeJSON(w, http.StatusOK, out)
}

type renameRequest struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// renameMe changes the name of the person the login is for (the surname is optional).
func (s *Server) renameMe(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	first, last := strings.Join(strings.Fields(req.FirstName), " "), strings.Join(strings.Fields(req.LastName), " ")
	switch {
	case first == "":
		writeError(w, http.StatusBadRequest, errors.New("Fill in your name."))
		return
	case utf8.RuneCountInString(first)+utf8.RuneCountInString(last) > 80:
		writeError(w, http.StatusBadRequest, errors.New("Use a shorter name (80 characters at most)."))
		return
	}
	account, _ := AccountFromContext(r.Context())
	person, ok := PersonFromContext(r.Context())
	if !ok || person == nil {
		writeError(w, http.StatusInternalServerError, errors.New("no person for this login"))
		return
	}
	person.FirstName, person.LastName = first, last
	if err := s.store.SavePerson(r.Context(), person); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, meResponse(account, person))
}

type passwordChange struct {
	Current  string `json:"current"`
	Password string `json:"password"`
}

// changeMyPassword sets a new password, given the current one (a login without one sets its first), and signs the
// login out everywhere but here. Wrong current passwords are counted like wrong sign-ins.
func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordChange
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, _ := AccountFromContext(r.Context())
	now := time.Now()
	key := "email:" + account.Email
	if account.PasswordHash != "" {
		if s.signins.count(key, signinWindow, now) >= signinMaxPerEmail {
			writeError(w, http.StatusTooManyRequests, errors.New("Too many tries. Wait 15 minutes and try again."))
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(req.Current)) != nil {
			s.signins.add(key, now)
			writeError(w, http.StatusBadRequest, errWrongPassword)
			return
		}
	}
	if err := validPassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	account.PasswordHash = string(hash)
	if err := s.store.SaveAccount(r.Context(), account); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.signins.clear(key)
	if st, ok := s.store.(domain.AccountSettingsStore); ok {
		if err := st.DeleteOtherSessions(r.Context(), account.ID, sessionToken(r)); err != nil {
			log.Printf("password change: sign out elsewhere: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// signOutOthers signs the login out everywhere but here.
func (s *Server) signOutOthers(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store.(domain.AccountSettingsStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, errNotAvailable)
		return
	}
	account, _ := AccountFromContext(r.Context())
	if err := st.DeleteOtherSessions(r.Context(), account.ID, sessionToken(r)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// askToChangeEmail sends a link to the new address. The answer is the same whether or not that address has a login of
// its own (it is told so by email instead), so this never says which addresses have one.
func (s *Server) askToChangeEmail(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, ok := s.store.(domain.AccountSettingsStore)
	if !ok || s.mailer == nil {
		writeError(w, http.StatusNotImplemented, errors.New("Changing your email isn’t available here."))
		return
	}
	account, _ := AccountFromContext(r.Context())
	email := domain.NormalizeEmail(req.Email)
	switch {
	case !emailRE.MatchString(email):
		writeError(w, http.StatusBadRequest, errors.New("That doesn’t look like an email address."))
		return
	case email == account.Email:
		writeError(w, http.StatusBadRequest, errors.New("That’s already your email."))
		return
	}
	if !s.mayMail(r, email) {
		writeError(w, http.StatusTooManyRequests, errors.New("We’ve sent a few emails to that address already. Try again in an hour."))
		return
	}
	if _, err := s.store.GetAccountByEmail(r.Context(), email); err == nil {
		if err := s.mailer.Send(r.Context(), email, "This email already has a login", emailTakenHTML(s.appURL)); err != nil {
			log.Printf("email change: tell an existing login: %v", err)
		}
		checkInbox(w, email)
		return
	}
	token, err := newToken()
	if err == nil {
		err = st.SaveEmailChange(r.Context(), account.ID, email, hashToken(token), time.Now().Add(domain.EmailChangeLife))
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	link := fmt.Sprintf("%s/change-email?token=%s", s.appURL, token)
	if err := s.mailer.Send(r.Context(), email, "Use this email for Minions?", emailChangeHTML(account.Email, link)); err != nil {
		log.Printf("email change: send: %v", err)
	}
	checkInbox(w, email)
}

// peekEmailChange says what a link would do, for the page that asks "Yes, use this email" (opening it changes nothing).
func (s *Server) peekEmailChange(w http.ResponseWriter, r *http.Request) {
	st, ok := s.store.(domain.AccountSettingsStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, errNotAvailable)
		return
	}
	accountID, email, err := st.PeekEmailChange(r.Context(), hashToken(r.URL.Query().Get("token")), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, errEmailLink)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errEmailLink)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email, "from": account.Email})
}

type tokenRequest struct {
	Token string `json:"token"`
}

// confirmEmailChange moves the login to the new address (opening the link proved it) and tells the old address.
func (s *Server) confirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st, ok := s.store.(domain.AccountSettingsStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, errNotAvailable)
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, errEmailLink)
		return
	}
	accountID, email, err := st.TakeEmailChange(r.Context(), hashToken(req.Token), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, errEmailLink)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errEmailLink)
		return
	}
	if other, err := s.store.GetAccountByEmail(r.Context(), email); err == nil && other.ID != account.ID {
		writeError(w, http.StatusConflict, errEmailTaken)
		return
	}
	old := account.Email
	account.Email, account.EmailVerified, account.VerificationToken = email, true, ""
	if err := s.store.SaveAccount(r.Context(), account); err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, errEmailTaken)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.mailer != nil && old != "" {
		if err := s.mailer.Send(r.Context(), old, "Your Minions email was changed", emailChangedHTML(email)); err != nil {
			log.Printf("email change: tell the old address: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"email": email})
}

// deleteMe deletes the login and everything its creator built: their agents (with what hangs on them), what the
// application keeps for them (WithAccountDeleteHook), their accounts on services and their own connections, then the
// login and its sessions. The person the login was for stays while they still chat with anyone's agents. Confirmed by
// typing the login's email.
func (s *Server) deleteMe(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, _ := AccountFromContext(r.Context())
	if account.Email == "" || domain.NormalizeEmail(req.Email) != account.Email {
		writeError(w, http.StatusBadRequest, errConfirmByEmail)
		return
	}
	ctx := r.Context()
	agents, err := s.store.ListAgents(ctx, account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, a := range agents {
		if a.CreatedBy != account.ID {
			continue
		}
		identityIDs := a.IdentityIDs
		if err := s.store.DeleteAgent(ctx, a.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.onAgentDelete != nil {
			go s.onAgentDelete(a.ID, identityIDs)
		}
	}
	if s.onAccountDelete != nil {
		if err := s.onAccountDelete(ctx, account.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	identities, err := s.store.ListIdentities(ctx, account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	for _, id := range identities {
		if id.CreatedBy == account.ID {
			if err := s.store.DeleteIdentity(ctx, id.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	integrations, err := s.store.ListIntegrations(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	deleter, _ := s.store.(interface {
		DeleteIntegration(ctx context.Context, id string) error
	})
	for _, in := range integrations {
		if in.CreatedBy == account.ID && deleter != nil {
			if err := deleter.DeleteIntegration(ctx, in.ID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	if err := s.store.DeleteAccount(ctx, account.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func emailChangeHTML(from, link string) string {
	l := html.EscapeString(link)
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>Use this email for Minions?</h2>
  <p>Someone signed in as %s asked to use this address for their Minions login instead. It changes only once you open the link and press "Yes, use this email". The link works for a day.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Open the link</a>
  <p style="margin-top:24px;font-size:13px;color:#666;">Or copy this link:<br><a href="%s">%s</a></p>
  <p style="font-size:12px;color:#999;margin-top:32px;">If it wasn't you, ignore this email: nothing changes.</p>
</body>
</html>`, html.EscapeString(from), l, l, l)
}

func emailTakenHTML(appURL string) string {
	l := html.EscapeString(appURL)
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>This email already has a login</h2>
  <p>Someone asked to move their Minions login to this address, but it already has a login of its own, so nothing changed. If that was you, sign in with this address instead, or use another one.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Sign in</a>
  <p style="font-size:12px;color:#999;margin-top:32px;">If it wasn't you, ignore this email.</p>
</body>
</html>`, l)
}

func emailChangedHTML(to string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>Your Minions email was changed</h2>
  <p>Your login now uses %s. You'll sign in with that address from now on.</p>
  <p style="font-size:12px;color:#999;margin-top:32px;">If this wasn't you, reply to this email so we can help.</p>
</body>
</html>`, html.EscapeString(to))
}
