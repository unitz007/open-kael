package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// createAgent only persists domain.Agent's storable fields (ID, Name,
// Description, Instructions, MaxIterations) — LLMs/Loop/Directory are live
// objects wired at hydration time, never through this API.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var agent domain.Agent
	if err := decodeJSON(r, &agent); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if agent.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	if user, ok := UserFromContext(r.Context()); ok {
		agent.CreatedBy = user.ID
	}

	if err := s.store.SaveAgent(r.Context(), &agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	var ownerID string
	if user, ok := UserFromContext(r.Context()); ok {
		ownerID = user.ID
	}
	list, err := s.store.ListAgents(r.Context(), ownerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

// getAgent returns the Agent with its Skills populated (via
// Store.LoadAgent).
func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.LoadAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !agentAccessible(r.Context(), agent) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !agentAccessible(r.Context(), agent) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	if err := s.store.DeleteAgent(r.Context(), agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setAgentIdentities replaces the full set of identity IDs linked to an agent.
// PUT /agents/{id}/identities — body: {"identity_ids": ["id-1", "id-2"]}
func (s *Server) setAgentIdentities(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.LoadAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !agentAccessible(r.Context(), agent) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	var body struct {
		IdentityIDs []string `json:"identity_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	oldIDs := agent.IdentityIDs
	agent.IdentityIDs = body.IdentityIDs
	if err := s.store.SaveAgent(r.Context(), agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentIdentityChange != nil {
		added := addedStrings(oldIDs, body.IdentityIDs)
		removed := removedStrings(oldIDs, body.IdentityIDs)
		if len(added) > 0 || len(removed) > 0 {
			s.onAgentIdentityChange(agent.ID, added, removed)
		}
	}
	writeJSON(w, http.StatusOK, agent)
}

// agentAccessible returns true when the requesting user may access the agent.
// When a user session is present, only the agent's owner may access it.
// When no session is in context (static-token or unauthenticated dev mode),
// all agents are accessible.
func agentAccessible(ctx context.Context, agent *domain.Agent) bool {
	user, ok := UserFromContext(ctx)
	if !ok {
		return true
	}
	return agent.CreatedBy == "" || agent.CreatedBy == user.ID
}

// addedStrings returns elements present in new but absent from old.
func addedStrings(old, new []string) []string {
	oldSet := make(map[string]struct{}, len(old))
	for _, id := range old {
		oldSet[id] = struct{}{}
	}
	var added []string
	for _, id := range new {
		if _, ok := oldSet[id]; !ok {
			added = append(added, id)
		}
	}
	return added
}

// removedStrings returns elements present in old but absent from new.
func removedStrings(old, new []string) []string {
	newSet := make(map[string]struct{}, len(new))
	for _, id := range new {
		newSet[id] = struct{}{}
	}
	var removed []string
	for _, id := range old {
		if _, ok := newSet[id]; !ok {
			removed = append(removed, id)
		}
	}
	return removed
}
