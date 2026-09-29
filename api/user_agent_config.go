package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

type userAgentConfigRequest struct {
	Instructions string `json:"instructions"`
}

// getMyAgentConfig handles GET /users/me/agents/{agentID}/config —
// returns the authenticated user's personal instructions for the given agent.
// Returns 404 when no config has been set yet.
func (s *Server) getMyAgentConfig(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	agentID := r.PathValue("agentID")
	if _, err := s.store.GetAgent(r.Context(), agentID); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, errors.New("agent not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg, err := s.store.GetUserAgentConfig(r.Context(), user.ID, agentID)
	if errors.Is(err, domain.ErrNotFound) {
		writeJSON(w, http.StatusOK, &domain.UserAgentConfig{UserID: user.ID, AgentID: agentID})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// putMyAgentConfig handles PUT /users/me/agents/{agentID}/config —
// sets or replaces the authenticated user's personal instructions for the agent.
func (s *Server) putMyAgentConfig(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	agentID := r.PathValue("agentID")
	if _, err := s.store.GetAgent(r.Context(), agentID); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, errors.New("agent not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var req userAgentConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid request body"))
		return
	}
	cfg := &domain.UserAgentConfig{UserID: user.ID, AgentID: agentID, Instructions: req.Instructions}
	if err := s.store.SetUserAgentConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
