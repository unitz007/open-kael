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
	list, err := s.store.ListSkillsByAgent(r.Context(), r.PathValue("agentID"))
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
	writeJSON(w, http.StatusOK, skill)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	skillID := r.PathValue("id")
	var agentID string
	if s.onSkillChange != nil {
		if sk, err := s.store.GetSkill(r.Context(), skillID); err == nil {
			agentID = sk.AgentID
		}
	}
	if err := s.store.DeleteSkill(r.Context(), skillID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.onSkillChange != nil && agentID != "" {
		s.onSkillChange(agentID)
	}
	w.WriteHeader(http.StatusNoContent)
}
