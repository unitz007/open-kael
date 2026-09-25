package api

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/unitz007/open-kael/domain"
)

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
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
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, errors.New("email and password are required"))
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	user := &domain.User{
		ID:           newID(),
		Email:        req.Email,
		PasswordHash: string(hash),
	}
	if err := s.store.SaveUser(r.Context(), user); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
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
