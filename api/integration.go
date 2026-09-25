package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// listIntegrations returns all integrations with their Tools, Identities, and
// Events populated — the full shape the frontend needs to build its picker UIs.
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
	writeJSON(w, http.StatusOK, integration)
}
