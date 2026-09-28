package api

import (
	"errors"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

type toolApprovalResponse struct {
	ToolID           string `json:"tool_id"`
	RequiresApproval bool   `json:"requires_approval"`
}

func (s *Server) listMyToolApprovals(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	approvals, err := s.store.ListUserToolApprovals(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]toolApprovalResponse, 0, len(approvals))
	for toolID := range approvals {
		out = append(out, toolApprovalResponse{ToolID: toolID, RequiresApproval: true})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setMyToolApproval(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	toolID := r.PathValue("toolID")
	if _, err := s.store.GetTool(r.Context(), toolID); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, errors.New("tool not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetUserToolApproval(r.Context(), user.ID, toolID, true); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toolApprovalResponse{ToolID: toolID, RequiresApproval: true})
}

func (s *Server) deleteMyToolApproval(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	toolID := r.PathValue("toolID")
	if err := s.store.SetUserToolApproval(r.Context(), user.ID, toolID, false); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
