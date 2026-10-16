package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// listIntegrations returns all integrations with their Tools, Identities, and
// Events populated — the full shape the frontend needs to build its picker UIs.
// Identities embedded in the response are filtered to those owned by the
// requesting creator plus platform-seeded ones (created_by = "").
func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListIntegrations(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]*domain.Integration, 0, len(rows))
	for _, row := range rows {
		if !integrationAccessible(r.Context(), row) {
			continue
		}
		full, err := s.store.LoadIntegration(r.Context(), row.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.integrationEvents != nil {
			full.Events = s.integrationEvents.EventsFor(full.Service)
		}
		full.Identities = scopedIdentities(r.Context(), full.Identities)
		full.Tools = enabledTools(full.Tools)
		out = append(out, full)
	}
	writeJSONList(w, http.StatusOK, out)
}

func (s *Server) getIntegration(w http.ResponseWriter, r *http.Request) {
	integration, err := s.store.LoadIntegration(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !integrationAccessible(r.Context(), integration) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	if s.integrationEvents != nil {
		integration.Events = s.integrationEvents.EventsFor(integration.Service)
	}
	integration.Identities = scopedIdentities(r.Context(), integration.Identities)
	integration.Tools = enabledTools(integration.Tools)
	writeJSON(w, http.StatusOK, integration)
}

// scopedIdentities filters a slice to identities accessible by the requesting
// creator: platform-seeded (CreatedBy=="") plus those owned by them. When no
// session is present (static-token or dev mode) all identities are returned.
func scopedIdentities(ctx context.Context, identities []*domain.Identity) []*domain.Identity {
	user, ok := UserFromContext(ctx)
	if !ok {
		return identities
	}
	out := make([]*domain.Identity, 0, len(identities))
	for _, id := range identities {
		if id.CreatedBy == "" || id.CreatedBy == user.ID {
			out = append(out, id)
		}
	}
	return out
}

// integrationAccessible reports whether the requesting creator may see an
// integration: platform-seeded ones (CreatedBy == "") are visible to all, a
// creator's own connection only to them. With no session (static-token or dev
// mode) everything is visible, matching scopedIdentities.
func integrationAccessible(ctx context.Context, i *domain.Integration) bool {
	if i.CreatedBy == "" {
		return true
	}
	user, ok := UserFromContext(ctx)
	if !ok {
		return true
	}
	return i.CreatedBy == user.ID
}

// IntegrationAccessible is the exported form for route plugins that serve
// integration-scoped data and must honour the same tenancy boundary.
func IntegrationAccessible(ctx context.Context, i *domain.Integration) bool {
	return integrationAccessible(ctx, i)
}

// enabledTools drops tools their owner turned off, so pickers built from the
// integration list never offer them. The tools endpoint still returns every
// tool, for the screen where they are managed.
func enabledTools(tools []*domain.ToolDefinition) []*domain.ToolDefinition {
	out := make([]*domain.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if !t.Disabled {
			out = append(out, t)
		}
	}
	return out
}
