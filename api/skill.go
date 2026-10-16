package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// createSkill sets AgentID from the URL path — POST
// /agents/{agentID}/skills always creates a Skill owned by that Agent,
// regardless of what the request body says.
func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var skill domain.Skill
	if err := decodeJSON(r, &skill); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if skill.ID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	skill.AgentID = r.PathValue("agentID")

	agent, err := s.store.GetAgent(r.Context(), skill.AgentID)
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

	if err := s.store.SaveSkill(r.Context(), &skill); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onSkillChange != nil {
		s.onSkillChange(skill.AgentID)
	}
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) listSkillsByAgent(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("agentID")
	agent, err := s.store.GetAgent(r.Context(), agentID)
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
	list, err := s.store.ListSkillsByAgent(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	skill, err := s.store.GetSkill(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.skillAgentAllowed(w, r, skill.AgentID) {
		return
	}
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetSkill(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.skillAgentAllowed(w, r, existing.AgentID) {
		return
	}
	var skill domain.Skill
	if err := decodeJSON(r, &skill); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	skill.ID = existing.ID
	skill.AgentID = existing.AgentID
	if err := s.store.SaveSkill(r.Context(), &skill); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onSkillChange != nil {
		s.onSkillChange(skill.AgentID)
	}
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	skillID := r.PathValue("id")
	sk, err := s.store.GetSkill(r.Context(), skillID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.skillAgentAllowed(w, r, sk.AgentID) {
		return
	}
	if err := s.store.DeleteSkill(r.Context(), skillID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onSkillChange != nil {
		s.onSkillChange(sk.AgentID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// skillAgentAllowed checks that the caller may touch a skill belonging to
// agentID, writing the error response and returning false when not. It fails
// closed: if the owning agent can't be loaded (missing, or a store error) the
// caller is refused, rather than skipping the ownership check — a skipped
// check is the multi-tenancy boundary quietly disappearing.
func (s *Server) skillAgentAllowed(w http.ResponseWriter, r *http.Request, agentID string) bool {
	agent, err := s.store.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, domain.ErrNotFound)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return false
	}
	if !agentAccessible(r.Context(), agent) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return false
	}
	return true
}
