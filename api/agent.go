package api

import (
	"context"
	"errors"
	"fmt"
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
	agent.ID = domain.NewID()
	if user, ok := UserFromContext(r.Context()); ok {
		agent.CreatedBy = user.ID
	}

	if err := s.store.SaveAgent(r.Context(), &agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentCreate != nil {
		s.onAgentCreate(agent.ID)
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
	identityIDs := agent.IdentityIDs
	if err := s.store.DeleteAgent(r.Context(), agent.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentDelete != nil && len(identityIDs) > 0 {
		go s.onAgentDelete(agent.ID, identityIDs)
	}
	w.WriteHeader(http.StatusNoContent)
}

// patchAgent updates mutable text fields on an agent (Greeting, Description,
// Instructions). Only fields present in the JSON body are updated.
// PATCH /agents/{id}
func (s *Server) patchAgent(w http.ResponseWriter, r *http.Request) {
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
	var patch struct {
		Greeting     *string `json:"greeting"`
		Description  *string `json:"description"`
		Instructions *string `json:"instructions"`
	}
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if patch.Greeting != nil {
		agent.Greeting = *patch.Greeting
	}
	if patch.Description != nil {
		agent.Description = *patch.Description
	}
	if patch.Instructions != nil {
		agent.Instructions = *patch.Instructions
	}
	if err := s.store.SaveAgent(r.Context(), agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentUpdate != nil {
		s.onAgentUpdate(agent.ID)
	}
	writeJSON(w, http.StatusOK, agent)
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
	for _, id := range body.IdentityIDs {
		// Reject any identity already claimed by a different agent.
		existing, err := s.store.GetAgentByIdentityID(r.Context(), id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if existing != nil && existing.ID != agent.ID {
			writeError(w, http.StatusConflict, fmt.Errorf("identity %q is already linked to another agent", id))
			return
		}
		// Reject any identity the requesting creator doesn't own.
		identity, err := s.store.GetIdentity(r.Context(), id)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeError(w, http.StatusBadRequest, fmt.Errorf("identity %q not found", id))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !identityAccessible(r.Context(), identity) {
			writeError(w, http.StatusForbidden, fmt.Errorf("identity %q is not accessible", id))
			return
		}
	}
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

// setAgentCommands replaces the full set of bot commands for an agent.
// PUT /agents/{id}/commands — body: [{"command":"help","description":"...","message":"..."}]
func (s *Server) setAgentCommands(w http.ResponseWriter, r *http.Request) {
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
	var commands []domain.BotCommand
	if err := decodeJSON(r, &commands); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agent.Commands = commands
	if err := s.store.SaveAgent(r.Context(), agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentCommandChange != nil {
		s.onAgentCommandChange(agent.ID, commands)
	}
	writeJSON(w, http.StatusOK, agent)
}

// getAgentRules returns the rules the creator has set on how this agent uses its tools.
// GET /agents/{id}/rules
func (s *Server) getAgentRules(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.accessibleAgent(w, r)
	if !ok {
		return
	}
	rules := agent.Rules
	if rules == nil {
		rules = []domain.ToolRule{}
	}
	writeJSON(w, http.StatusOK, rules)
}

// setAgentRules replaces the full set of rules for an agent.
// PUT /agents/{id}/rules — body: [{"tool_id":"…","input":"size","kind":"max","value":100000,"message":"…"}]
//
// Every rule is checked against the tool it names (the input must exist and the value must fit
// it) and the tool must be one the creator may use; a set that can't all hold is refused with a
// reason in plain words. Saving reloads the agent, so the rules apply to the next message.
func (s *Server) setAgentRules(w http.ResponseWriter, r *http.Request) {
	agent, ok := s.accessibleAgent(w, r)
	if !ok {
		return
	}
	var rules []domain.ToolRule
	if err := decodeJSON(r, &rules); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tools := make(map[string]*domain.ToolDefinition, len(rules))
	for _, rule := range rules {
		if _, seen := tools[rule.ToolID]; seen {
			continue
		}
		tool, err := s.store.GetTool(r.Context(), rule.ToolID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeError(w, http.StatusBadRequest, errors.New("one of the rules is for something that doesn't exist"))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		integration, err := s.store.LoadIntegration(r.Context(), tool.IntegrationID)
		if err != nil || !integrationAccessible(r.Context(), integration) {
			// A tool the creator can't see is a tool that doesn't exist, as everywhere else.
			writeError(w, http.StatusBadRequest, errors.New("one of the rules is for something that doesn't exist"))
			return
		}
		tools[rule.ToolID] = tool
	}
	normalized, err := domain.NormalizeRules(rules, tools)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	for i := range normalized {
		if normalized[i].ID == "" {
			normalized[i].ID = domain.NewID()
		}
	}
	agent.Rules = normalized
	if err := s.store.SaveAgent(r.Context(), agent); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onAgentUpdate != nil {
		s.onAgentUpdate(agent.ID)
	}
	writeJSON(w, http.StatusOK, agent)
}

// accessibleAgent loads the agent in the path and checks the requester may use it; on failure it
// has already written the answer.
func (s *Server) accessibleAgent(w http.ResponseWriter, r *http.Request) (*domain.Agent, bool) {
	agent, err := s.store.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, err)
		return nil, false
	}
	if !agentAccessible(r.Context(), agent) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return nil, false
	}
	return agent, true
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
