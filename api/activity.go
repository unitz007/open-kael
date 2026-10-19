package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// WithActivity lets creators see how their agents are doing: counts, never what anyone wrote.
func WithActivity(r domain.ActivityReader) Option {
	return func(s *Server) { s.activity = r }
}

// getActivity returns the numbers for one of the creator's agents (?agent_id=) or for all of
// them, over ?days= (1 to 30, default 14). A creator only ever sees agents they created: an agent
// the platform provides, or someone else's, is not theirs to look at.
func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	if s.activity == nil {
		writeError(w, http.StatusNotImplemented, errors.New("activity is not available"))
		return
	}
	days := 14
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			writeError(w, http.StatusBadRequest, errors.New("days must be a number from 1 to 30"))
			return
		}
		days = n
	}
	user, signedIn := UserFromContext(r.Context())
	if !signedIn && s.authToken == "" {
		// Other creator routes run open when no key is configured (a dev default); how people are
		// doing is never open to anyone who asks.
		writeError(w, http.StatusUnauthorized, errUnauthorized)
		return
	}
	mine := func(a *domain.Agent) bool { return !signedIn || (a.CreatedBy != "" && a.CreatedBy == user.ID) }

	var ids []string
	if id := r.URL.Query().Get("agent_id"); id != "" {
		agent, err := s.store.LoadAgent(r.Context(), id)
		if err != nil || !mine(agent) {
			writeError(w, http.StatusNotFound, domain.ErrNotFound)
			return
		}
		ids = []string{agent.ID}
	} else {
		var ownerID string
		if signedIn {
			ownerID = user.ID
		}
		list, err := s.store.ListAgents(r.Context(), ownerID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for _, a := range list {
			if mine(a) {
				ids = append(ids, a.ID)
			}
		}
	}
	sum, err := s.activity.ActivitySummary(r.Context(), ids, days, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}
