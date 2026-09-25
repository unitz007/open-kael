package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

type setupItem struct {
	Identity         *domain.Identity         `json:"identity"`
	Integration      *domain.Integration      `json:"integration"`
	Authorization    *domain.AppAuthorization `json:"authorization,omitempty"`
	RequiredBySkills []string                 `json:"required_by_skills,omitempty"`
}

type setupResponse struct {
	Agent  *domain.Agent `json:"agent,omitempty"`
	Setups []setupItem   `json:"setups"`
}

// getMySetup returns the agent the user is linked to (via their messenger
// channels) and, for each of that agent's identities, the matching
// integration and any existing AppAuthorization the user has granted.
func (s *Server) getMySetup(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())

	channels, err := s.store.ListMessengerChannelsByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(channels) == 0 {
		writeJSON(w, http.StatusOK, setupResponse{Setups: []setupItem{}})
		return
	}

	agentShell, err := s.store.GetAgentByIdentityID(r.Context(), channels[0].IdentityID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeJSON(w, http.StatusOK, setupResponse{Setups: []setupItem{}})
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// LoadAgent populates Skills (GetAgentByIdentityID leaves them nil).
	agent, err := s.store.LoadAgent(r.Context(), agentShell.ID)
	if err != nil {
		agent = agentShell // fallback — skills will be empty, required_by_skills won't be populated
	}

	// Build a map from integration ID → skill names that require it, by
	// resolving each skill's tool bindings to their ToolDefinition.IntegrationID.
	integrationSkills := map[string][]string{}
	for _, skill := range agent.Skills {
		seen := map[string]bool{}
		for _, tb := range skill.Tools {
			tool, err := s.store.GetTool(r.Context(), tb.ToolID)
			if err != nil || seen[tool.IntegrationID] {
				continue
			}
			seen[tool.IntegrationID] = true
			integrationSkills[tool.IntegrationID] = append(integrationSkills[tool.IntegrationID], skill.Name)
		}
	}

	setups := make([]setupItem, 0, len(agent.IdentityIDs))
	for _, identityID := range agent.IdentityIDs {
		identity, err := s.store.GetIdentity(r.Context(), identityID)
		if err != nil {
			continue
		}
		integration, err := s.store.LoadIntegration(r.Context(), identity.IntegrationID)
		if err != nil {
			continue
		}
		var auth *domain.AppAuthorization
		if a, err := s.store.GetAppAuthorizationByUserAndIdentity(r.Context(), user.ID, identityID); err == nil {
			auth = a
		}
		setups = append(setups, setupItem{
			Identity:         identity,
			Integration:      integration,
			Authorization:    auth,
			RequiredBySkills: integrationSkills[identity.IntegrationID],
		})
	}

	writeJSON(w, http.StatusOK, setupResponse{Agent: agent, Setups: setups})
}

type createAuthorizationRequest struct {
	IdentityID     string `json:"identity_id"`
	ExternalUserID string `json:"external_user_id"`
	Credential     string `json:"credential"`
	Name           string `json:"name"`
}

// createMyAppAuthorization upserts an AppAuthorization for the requesting
// user. Handles both plain-ID integrations (e.g. FPL team ID stored in
// ExternalUserID) and credential-based integrations (e.g. FPL refresh token
// stored encrypted in CredentialRef).
func (s *Server) createMyAppAuthorization(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())

	var req createAuthorizationRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.IdentityID == "" {
		writeError(w, http.StatusBadRequest, errors.New("identity_id is required"))
		return
	}

	existing, err := s.store.GetAppAuthorizationByUserAndIdentity(r.Context(), user.ID, req.IdentityID)
	var auth domain.AppAuthorization
	if err == nil {
		auth = *existing
		if req.ExternalUserID != "" {
			auth.ExternalUserID = req.ExternalUserID
		}
		if req.Name != "" {
			auth.Name = req.Name
		}
	} else if errors.Is(err, domain.ErrNotFound) {
		auth = domain.AppAuthorization{
			ID:             newID(),
			IdentityID:     req.IdentityID,
			UserID:         user.ID,
			Scope:          domain.AppAuthorizationScopeUser,
			Name:           req.Name,
			ExternalUserID: req.ExternalUserID,
		}
	} else {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if req.Credential != "" {
		if s.encryptor == nil {
			writeError(w, http.StatusNotImplemented, errors.New("credential storage requires TOKEN_ENCRYPTION_KEY to be configured"))
			return
		}
		encrypted, err := s.encryptor.EncryptToken(r.Context(), req.Credential)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		auth.CredentialRef = encrypted
	}

	if err := s.store.SaveAppAuthorization(r.Context(), &auth); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, &auth)
}

func (s *Server) listMyAppAuthorizations(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	list, err := s.store.ListAppAuthorizationsByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

func (s *Server) deleteMyAppAuthorization(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	id := r.PathValue("id")

	existing, err := s.store.GetAppAuthorization(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if existing.UserID != user.ID {
		writeError(w, http.StatusForbidden, errors.New("forbidden"))
		return
	}
	if err := s.store.DeleteAppAuthorization(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
