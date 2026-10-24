package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

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
	if !cleanSkillTitle(w, &skill) {
		return
	}

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
	if !s.skillRefsAccessible(w, r, &skill) {
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
	if !cleanSkillTitle(w, &skill) {
		return
	}
	if !s.skillRefsAccessible(w, r, &skill) {
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

// cleanSkillTitle tidies a capability's title and refuses one that is too long.
func cleanSkillTitle(w http.ResponseWriter, skill *domain.Skill) bool {
	skill.Title = domain.CleanSkillTitle(skill.Title)
	if utf8.RuneCountInString(skill.Title) > domain.MaxSkillTitle {
		writeError(w, http.StatusBadRequest, fmt.Errorf("keep the name short: %d characters at most", domain.MaxSkillTitle))
		return false
	}
	return true
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

// skillRefsAccessible rejects a skill that grants an integration (or binds a
// tool of one) the requesting creator may not see. Without it a creator could
// name another creator's private connection and run it with that creator's
// key. Unknown references are reported the same way as inaccessible ones so
// the response never confirms that someone else's connection exists.
func (s *Server) skillRefsAccessible(w http.ResponseWriter, r *http.Request, skill *domain.Skill) bool {
	// A limit only means something on a connection the capability actually uses.
	listed := make(map[string]bool, len(skill.Integrations))
	for _, id := range skill.Integrations {
		listed[id] = true
	}
	for _, id := range skill.LimitedIntegrations {
		if !listed[id] {
			writeError(w, http.StatusBadRequest, errors.New("a limit refers to a connection this capability doesn't use"))
			return false
		}
	}
	ok, err := s.refsAccessible(r.Context(), skill)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return false
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("this skill refers to a connection that doesn't exist or isn't yours"))
		return false
	}
	if len(skill.Prerequisites) > 0 || len(skill.Rules) > 0 {
		// What must happen first, and the capability's own limits, are only on abilities it really grants
		// (domain/prerequisite.go, domain/tool_rule.go).
		toolsByID := map[string]*domain.ToolDefinition{}
		for _, id := range skill.Integrations {
			list, err := s.store.ListToolsByIntegration(r.Context(), id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return false
			}
			for _, t := range list {
				toolsByID[t.ID] = t
			}
		}
		for _, b := range skill.Tools {
			if t, err := s.store.GetTool(r.Context(), b.ToolID); err == nil {
				toolsByID[t.ID] = t
			}
		}
		granted := map[string]*domain.ToolDefinition{}
		for _, t := range domain.GrantedTools(skill, toolsByID) {
			granted[t.ID] = t
		}
		ps, err := domain.NormalizePrerequisites(skill.Prerequisites, granted)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return false
		}
		for i := range ps {
			if ps[i].ID == "" {
				ps[i].ID = fmt.Sprintf("p%d", i+1)
			}
		}
		skill.Prerequisites = ps
		rules, err := domain.NormalizeRules(skill.Rules, granted)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return false
		}
		for i := range rules {
			if rules[i].ID == "" {
				rules[i].ID = domain.NewID()
			}
		}
		if len(rules) == 0 {
			rules = nil
		}
		skill.Rules = rules
	}
	return true
}

func (s *Server) refsAccessible(ctx context.Context, skill *domain.Skill) (bool, error) {
	integrationOK := func(id string) (bool, error) {
		i, err := s.store.LoadIntegration(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return integrationAccessible(ctx, i), nil
	}
	for _, id := range skill.Integrations {
		if ok, err := integrationOK(id); err != nil || !ok {
			return false, err
		}
	}
	for _, b := range skill.Tools {
		tool, err := s.store.GetTool(ctx, b.ToolID)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if ok, err := integrationOK(tool.IntegrationID); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
