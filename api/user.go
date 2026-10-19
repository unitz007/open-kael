package api

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/unitz007/open-kael/domain"
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

var emailRE = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

type registerRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type sessionResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) registerUser(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case req.Email == "" || req.Password == "" || req.FirstName == "" || req.LastName == "":
		writeError(w, http.StatusBadRequest, errors.New("email, password, first_name, and last_name are required"))
		return
	case !emailRE.MatchString(req.Email):
		writeError(w, http.StatusBadRequest, errors.New("invalid email address"))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	verificationToken, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	emailVerified := s.mailer == nil // auto-verify when no mailer is configured
	// A new login comes with the person it is for, who carries the name.
	person := &domain.Person{ID: newID(), FirstName: req.FirstName, LastName: req.LastName}
	account := &domain.Account{
		ID:                newID(),
		Email:             req.Email,
		PasswordHash:      string(hash),
		EmailVerified:     emailVerified,
		VerificationToken: verificationToken,
		PersonID:          person.ID,
	}
	if err := s.store.SavePerson(r.Context(), person); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SaveAccount(r.Context(), account); err != nil {
		_ = s.store.DeletePerson(r.Context(), person.ID) // nobody is left behind by a refused sign-up
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, errors.New("an account with this email already exists"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if s.mailer != nil {
		link := fmt.Sprintf("%s/verify?token=%s", s.appURL, verificationToken)
		body := verificationEmailHTML(req.FirstName, link)
		if err := s.mailer.Send(r.Context(), req.Email, "Verify your email address", body); err != nil {
			log.Printf("email verification: send to %s: %v", req.Email, err)
		}
	}

	writeJSON(w, http.StatusCreated, meResponse(account, person))
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("token is required"))
		return
	}

	account, err := s.store.GetAccountByVerificationToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid or expired token"))
		return
	}
	if account.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]string{"message": "email already verified"})
		return
	}

	account.EmailVerified = true
	account.VerificationToken = ""
	if err := s.store.SaveAccount(r.Context(), account); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "email verified"})
}

func verificationEmailHTML(firstName, link string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<body style="font-family:sans-serif;max-width:480px;margin:40px auto;color:#111;">
  <h2>Hi %s, verify your email</h2>
  <p>Click the button below to verify your email address and activate your account.</p>
  <a href="%s" style="display:inline-block;padding:12px 24px;background:#000;color:#fff;text-decoration:none;border-radius:6px;font-weight:600;">Verify Email</a>
  <p style="margin-top:24px;font-size:13px;color:#666;">Or copy this link:<br><a href="%s">%s</a></p>
  <p style="font-size:12px;color:#999;margin-top:32px;">If you didn't create an account, you can safely ignore this email.</p>
</body>
</html>`, firstName, link, link, link)
}

func (s *Server) loginUser(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	account, err := s.store.GetAccountByEmail(r.Context(), req.Email)
	if err != nil {
		// Return the same error for wrong email or wrong password to prevent
		// user enumeration.
		writeError(w, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	if !account.EmailVerified {
		writeError(w, http.StatusForbidden, errors.New("email address not verified — check your inbox"))
		return
	}

	token, err := newToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sess := &domain.Session{
		Token:     token,
		AccountID: account.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	if err := s.store.SaveSession(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{Token: sess.Token, ExpiresAt: sess.ExpiresAt})
}

// me is how the signed-in person is described to the web app: the login and the person it is for.
type me struct {
	ID            string `json:"id"` // the login's id
	PersonID      string `json:"person_id,omitempty"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	FirstName     string `json:"first_name,omitempty"`
	LastName      string `json:"last_name,omitempty"`
}

func meResponse(a *domain.Account, p *domain.Person) me {
	m := me{ID: a.ID, PersonID: a.PersonID, Email: a.Email, EmailVerified: a.EmailVerified}
	if p != nil {
		m.PersonID, m.FirstName, m.LastName = p.ID, p.FirstName, p.LastName
	}
	return m
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	account, _ := AccountFromContext(r.Context())
	person, _ := PersonFromContext(r.Context())
	writeJSON(w, http.StatusOK, meResponse(account, person))
}

func (s *Server) logoutUser(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	token := r.Header.Get("Authorization")[len(prefix):]
	if err := s.store.DeleteSession(r.Context(), token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newID() string { return domain.NewID() }

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
