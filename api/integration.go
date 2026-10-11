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
		full, err := s.store.LoadIntegration(r.Context(), row.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if s.integrationEvents != nil {
			full.Events = s.integrationEvents.EventsFor(full.Service)
		}
			full.Identities = scopedIdentities(r.Context(), full.Identities)
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
	if s.integrationEvents != nil {
		integration.Events = s.integrationEvents.EventsFor(integration.Service)
	}
	integration.Identities = scopedIdentities(r.Context(), integration.Identities)
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
