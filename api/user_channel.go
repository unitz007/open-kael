package api

import (
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

func (s *Server) listMyChannels(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	list, err := s.store.ListMessengerChannelsByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONList(w, http.StatusOK, list)
}

func (s *Server) deleteMyChannel(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	id := r.PathValue("id")

	channels, err := s.store.ListMessengerChannelsByUser(r.Context(), user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	found := false
	for _, ch := range channels {
		if ch.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, domain.ErrNotFound)
		return
	}
	if err := s.store.DeleteMessengerChannel(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
