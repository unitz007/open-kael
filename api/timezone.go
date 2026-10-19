package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// timezoneStore is what remembering a time zone needs from storage (Postgres has it).
type timezoneStore interface {
	UserTimezone(ctx context.Context, userID string) (string, error)
	SetUserTimezone(ctx context.Context, userID, timezone string) error
}

// rememberMyTimezone records the time zone a person's own device reports, but only when they have
// none: a zone they chose themselves is never overwritten by a guess. The browser knows it without
// asking anyone anything, so the creator app calls this quietly once they are signed in.
func (s *Server) rememberMyTimezone(w http.ResponseWriter, r *http.Request) {
	store, ok := s.store.(timezoneStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("time zones are not available"))
		return
	}
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errUnauthorized)
		return
	}
	var body struct {
		Timezone string `json:"timezone"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	zone := strings.TrimSpace(body.Timezone)
	if !validIANAZone(zone) {
		writeError(w, http.StatusBadRequest, errors.New("that is not a time zone name"))
		return
	}
	current, err := store.UserTimezone(r.Context(), user.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if current != "" {
		writeJSON(w, http.StatusOK, map[string]any{"timezone": current, "saved": false})
		return
	}
	if err := store.SetUserTimezone(r.Context(), user.ID, zone); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"timezone": zone, "saved": true})
}

// validIANAZone accepts a real zone name and nothing else ("Local" means the server's own).
func validIANAZone(name string) bool {
	if name == "" || len(name) > 64 || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
