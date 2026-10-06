package api

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/unitz007/open-kael/domain"
)

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
	user := &domain.User{
		ID:                newID(),
		Email:             req.Email,
		FirstName:         req.FirstName,
		LastName:          req.LastName,
		PasswordHash:      string(hash),
		EmailVerified:     emailVerified,
		VerificationToken: verificationToken,
	}
	if err := s.store.SaveUser(r.Context(), user); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if s.mailer != nil {
		link := fmt.Sprintf("%s/verify?token=%s", s.appURL, verificationToken)
		body := verificationEmailHTML(req.FirstName, link)
		if err := s.mailer.Send(r.Context(), req.Email, "Verify your email address", body); err != nil {
			// Don't fail the signup — log and let the user request a resend later.
			_ = err
		}
	}

	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("token is required"))
		return
	}

	user, err := s.store.GetUserByVerificationToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid or expired token"))
		return
	}
	if user.EmailVerified {
		writeJSON(w, http.StatusOK, map[string]string{"message": "email already verified"})
		return
	}

	user.EmailVerified = true
	user.VerificationToken = ""
	if err := s.store.SaveUser(r.Context(), user); err != nil {
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

	user, err := s.store.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		// Return the same error for wrong email or wrong password to prevent
		// user enumeration.
		writeError(w, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid credentials"))
		return
	}
	if !user.EmailVerified {
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
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}
	if err := s.store.SaveSession(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{Token: sess.Token, ExpiresAt: sess.ExpiresAt})
}

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	writeJSON(w, http.StatusOK, user)
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
