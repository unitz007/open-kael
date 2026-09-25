package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

func (s *Server) listToolsByIntegration(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, tool)
}
