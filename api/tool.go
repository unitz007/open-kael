package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

func (s *Server) listToolsByIntegration(w http.ResponseWriter, r *http.Request) {
	if !s.integrationVisible(r, r.PathValue("integrationID")) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	list, err := s.store.ListToolsByIntegration(r.Context(), r.PathValue("integrationID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

func (s *Server) getTool(w http.ResponseWriter, r *http.Request) {
	tool, err := s.store.GetTool(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !s.integrationVisible(r, tool.IntegrationID) {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, tool)
}

// integrationVisible reports whether the requester may see an integration's
// tools. An unknown integration is treated as visible so the usual empty or
// not-found answer is produced by the caller.
func (s *Server) integrationVisible(r *http.Request, integrationID string) bool {
	i, err := s.store.LoadIntegration(r.Context(), integrationID)
	if err != nil {
		return true
	}
	return integrationAccessible(r.Context(), i)
}
