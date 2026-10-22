package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/unitz007/open-kael/domain"
)

// Getting back into a login: sending the confirmation link again, and choosing a new password with a one-time link
// sent by email. Every answer is the same whether or not the address has a login, so none of this tells anyone which
// addresses do; and tries are counted, so guessing is slow.

const (
	signinWindow      = 15 * time.Minute
	signinMaxPerEmail = 10 // failed sign-ins for one address in the window
	signinMaxPerFrom  = 50 // failed sign-ins from one place in the window
	mailWindow        = time.Hour
	mailMaxPerEmail   = 3  // emails of these kinds to one address in the window
	mailMaxPerFrom    = 20 // asked for from one place in the window
)

var (
	errTooManySignins = errors.New("Too many tries. Wait 15 minutes and try again, or choose a new password.")
	errPasswordShort  = fmt.Errorf("Use at least %d characters for your password.", domain.MinPasswordLength)
	errResetLink      = errors.New("This link has run out or was already used. Ask for a new one.")
)

// attempts counts recent tries per key (an address, or where requests come from). It is in memory: a restart forgets.
type attempts struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// count is how many tries the key made in the window.
func (a *attempts) count(key string, window time.Duration, now time.Time) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.seen[key][:0]
	for _, t := range a.seen[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.seen, key)
		return 0
	}
	a.seen[key] = kept
	return len(kept)
}

func (a *attempts) add(key string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string][]time.Time{}
	}
	if len(a.seen) > 100_000 { // never grows without bound
		a.seen = map[string][]time.Time{}
	}
	a.seen[key] = append(a.seen[key], now)
}

func (a *attempts) clear(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, key)
}

// requestFrom is where a request came from, for counting tries (Fly puts the visitor's address in a header).
func requestFrom(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// mayMail reports whether another email of these kinds may go to this address (and counts it when it may).
func (s *Server) mayMail(r *http.Request, email string) bool {
	now := time.Now()
	from := "from:" + requestFrom(r)
	if s.mails.count("to:"+email, mailWindow, now) >= mailMaxPerEmail || s.mails.count(from, mailWindow, now) >= mailMaxPerFrom {
		return false
	}
	s.mails.add("to:"+email, now)
	s.mails.add(from, now)
	return true
}

func validPassword(p string) error {
	if utf8.RuneCountInString(p) < domain.MinPasswordLength {
		return errPasswordShort
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type emailRequest struct {
	Email string `json:"email"`
}

// checkInbox is the one answer to anything that may send an email: it never says whether the address has a login.
func checkInbox(w http.ResponseWriter, email string) {
	writeJSON(w, http.StatusAccepted, map[string]any{"email": email, "check_inbox": true})
}

// resendVerification sends the link to confirm an address again, to a login that has not confirmed it yet.
func (s *Server) resendVerification(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	email := domain.NormalizeEmail(req.Email)
	if s.mailer != nil && emailRE.MatchString(email) && s.mayMail(r, email) {
		if account, err := s.store.GetAccountByEmail(r.Context(), email); err == nil && !account.EmailVerified {
			s.sendVerification(r.Context(), account, "")
		}
	}
	checkInbox(w, email)
}

// sendVerification sends the link to confirm a login's address, making one if it has none.
func (s *Server) sendVerification(ctx context.Context, account *domain.Account, firstName string) {
	if account.VerificationToken == "" {
		token, err := newToken()
		if err != nil {
			return
		}
		account.VerificationToken = token
		if err := s.store.SaveAccount(ctx, account); err != nil {
			log.Printf("email verification: save a link: %v", err)
			return
		}
	}
	if firstName == "" && account.PersonID != "" {
		if p, err := s.store.GetPerson(ctx, account.PersonID); err == nil {
			firstName = p.FirstName
		}
	}
	link := fmt.Sprintf("%s/verify?token=%s", s.appURL, account.VerificationToken)
	if err := s.mailer.Send(ctx, account.Email, "Confirm your email", verificationEmailHTML(firstName, link)); err != nil {
		log.Printf("email verification: send: %v", err)
	}
}

// answerTakenRegistration is what registering with an address that already has a login does, while the person asking
// is told the same as for a new one. A confirmed login's owner is told someone tried and how to get in; a login that
// was never confirmed is not trusted, so it takes the new password and the confirmation link is sent again (whoever
// can open it owns the address).
func (s *Server) answerTakenRegistration(r *http.Request, existing *domain.Account, passwordHash, firstName string) {
	if !s.mayMail(r, existing.Email) {
		return
	}
	ctx := r.Context()
	if existing.EmailVerified {
		if err := s.mailer.Send(ctx, existing.Email, "You already have an account", alreadyRegisteredEmailHTML(s.appURL)); err != nil {
			log.Printf("registration: tell an existing login: %v", err)
		}
		return
	}
	existing.PasswordHash = passwordHash
	existing.VerificationToken = ""
	s.sendVerification(ctx, existing, firstName)
}

// forgotPassword sends a one-time link to choose a new password, when the address has a login.
func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	email := domain.NormalizeEmail(req.Email)
	resets, ok := s.store.(domain.PasswordResetStore)
	if !ok || s.mailer == nil {
		writeError(w, http.StatusNotImplemented, errors.New("Choosing a new password isn’t available here."))
		return
	}
	if emailRE.MatchString(email) && s.mayMail(r, email) {
		if account, err := s.store.GetAccountByEmail(r.Context(), email); err == nil {
			token, err := newToken()
			if err == nil {
				err = resets.SavePasswordReset(r.Context(), account.ID, hashToken(token), time.Now().Add(domain.PasswordResetLife))
			}
			if err != nil {
				log.Printf("password reset: make a link: %v", err)
			} else {
				link := fmt.Sprintf("%s/reset?token=%s", s.appURL, token)
				if err := s.mailer.Send(r.Context(), account.Email, "Choose a new password", resetEmailHTML(link)); err != nil {
					log.Printf("password reset: send: %v", err)
				}
			}
		}
	}
	checkInbox(w, email)
}

type resetRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// resetPassword uses a one-time link to set a new password. Opening the link proves the address, so it is confirmed
// too, and the login is signed out everywhere else.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	resets, ok := s.store.(domain.PasswordResetStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("Choosing a new password isn’t available here."))
		return
	}
	if err := validPassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeError(w, http.StatusBadRequest, errResetLink)
		return
	}
	accountID, err := resets.TakePasswordReset(r.Context(), hashToken(req.Token), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, errResetLink)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errResetLink)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	account.PasswordHash = string(hash)
	account.EmailVerified = true
	account.VerificationToken = ""
	if err := s.store.SaveAccount(r.Context(), account); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := resets.DeleteSessionsOf(r.Context(), account.ID); err != nil {
		log.Printf("password reset: sign out elsewhere: %v", err)
	}
	s.signins.clear("email:" + account.Email)
	writeJSON(w, http.StatusOK, map[string]string{"email": account.Email})
}

func resetEmailHTML(link string) string {
	l := html.EscapeString(link)
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>Choose a new password</h2>
  <p>Someone, hopefully you, asked to choose a new password for this email. The link works once, for an hour.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Choose a new password</a>
  <p style="margin-top:24px;font-size:13px;color:#666;">Or copy this link:<br><a href="%s">%s</a></p>
  <p style="font-size:12px;color:#999;margin-top:32px;">If it wasn't you, ignore this email: your password stays as it is.</p>
</body>
</html>`, l, l, l)
}

func alreadyRegisteredEmailHTML(appURL string) string {
	l := html.EscapeString(appURL)
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>You already have an account</h2>
  <p>Someone tried to create an account with this email, but it already has one. If that was you, sign in instead. If you've forgotten your password, choose "Forgot password?" there.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Sign in</a>
  <p style="font-size:12px;color:#999;margin-top:32px;">If it wasn't you, ignore this email: nothing has changed.</p>
</body>
</html>`, l)
}
