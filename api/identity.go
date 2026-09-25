package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// errEncryptorRequired is returned when a provider-specific identity endpoint
// is called but no CredentialEncryptor was configured on the server.
// ErrEncryptorRequired is returned when a provider-specific identity endpoint
// is called but no CredentialEncryptor was configured on the server.
var ErrEncryptorRequired = errors.New("credential encryption not configured — set TOKEN_ENCRYPTION_KEY")

var errEncryptorRequired = ErrEncryptorRequired

type createTelegramIdentityRequest struct {
	IntegrationID string `json:"integration_id"`
	Name          string `json:"name"`
	Token         string `json:"token"`
}

type createSlackIdentityRequest struct {
	IntegrationID string `json:"integration_id"`
	Name          string `json:"name"`
	BotToken      string `json:"bot_token"`
}

type createGitHubAppIdentityRequest struct {
	IntegrationID string `json:"integration_id"`
	Name          string `json:"name"`
	AppID         string `json:"app_id"`
	PrivateKey    string `json:"private_key"`
}

type createOAuthIdentityRequest struct {
	IntegrationID string `json:"integration_id"`
	Name          string `json:"name"`
}

func (s *Server) createTelegramIdentity(w http.ResponseWriter, r *http.Request) {
	var req createTelegramIdentityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, errors.New("name and token are required"))
		return
	}
	ref, err := s.encryptCredential(w, r, req.Token)
	if err != nil {
		return
	}
	identity := &domain.Identity{
		ID:            newID(),
		Name:          req.Name,
		IntegrationID: req.IntegrationID,
		Kind:          domain.IdentityKindBot,
		CredentialRef: ref,
	}
	if err := s.store.SaveIdentity(r.Context(), identity); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, identity)
}

func (s *Server) createSlackIdentity(w http.ResponseWriter, r *http.Request) {
	var req createSlackIdentityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.BotToken == "" {
		writeError(w, http.StatusBadRequest, errors.New("name and bot_token are required"))
		return
	}
	ref, err := s.encryptCredential(w, r, req.BotToken)
	if err != nil {
		return
	}
	identity := &domain.Identity{
		ID:            newID(),
		Name:          req.Name,
		IntegrationID: req.IntegrationID,
		Kind:          domain.IdentityKindBot,
		CredentialRef: ref,
	}
	if err := s.store.SaveIdentity(r.Context(), identity); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, identity)
}

func (s *Server) createGitHubAppIdentity(w http.ResponseWriter, r *http.Request) {
	var req createGitHubAppIdentityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.AppID == "" || req.PrivateKey == "" {
		writeError(w, http.StatusBadRequest, errors.New("name, app_id, and private_key are required"))
		return
	}
	ref, err := s.encryptCredential(w, r, req.PrivateKey)
	if err != nil {
		return
	}
	identity := &domain.Identity{
		ID:            newID(),
		Name:          req.Name,
		IntegrationID: req.IntegrationID,
		Kind:          domain.IdentityKindBot,
		AppID:         req.AppID,
		CredentialRef: ref,
	}
	if err := s.store.SaveIdentity(r.Context(), identity); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, identity)
}

func (s *Server) listIdentities(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListIdentities(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

func (s *Server) getIdentity(w http.ResponseWriter, r *http.Request) {
	identity, err := s.store.GetIdentity(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, identity)
}

func (s *Server) deleteIdentity(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteIdentity(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createOAuthIdentity creates a credential-free OAuth identity — a named
// placeholder that users connect to via an OAuth flow. Used for integrations
// like FPL that have no bot token; per-user credentials are stored as
// AppAuthorizations referencing this identity's ID.
func (s *Server) createOAuthIdentity(w http.ResponseWriter, r *http.Request) {
	var req createOAuthIdentityRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.IntegrationID == "" {
		writeError(w, http.StatusBadRequest, errors.New("name and integration_id are required"))
		return
	}
	identity := &domain.Identity{
		ID:            newID(),
		Name:          req.Name,
		IntegrationID: req.IntegrationID,
		Kind:          domain.IdentityKindOAuth,
	}
	if err := s.store.SaveIdentity(r.Context(), identity); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, identity)
}

// encryptCredential encrypts plaintext using the server's encryptor. On
// error it writes the appropriate HTTP response and returns a non-nil error
// so the caller can return immediately.
func (s *Server) encryptCredential(w http.ResponseWriter, r *http.Request, plaintext string) (string, error) {
	if s.encryptor == nil {
		writeError(w, http.StatusNotImplemented, errEncryptorRequired)
		return "", errEncryptorRequired
	}
	ref, err := s.encryptor.EncryptToken(r.Context(), plaintext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return "", err
	}
	return ref, nil
}
