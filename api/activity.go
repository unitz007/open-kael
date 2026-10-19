package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// WithActivity lets creators see how their agents are doing: counts, never what anyone wrote.
func WithActivity(r domain.ActivityReader) Option {
	return func(s *Server) { s.activity = r }
}

// activityScope reads and checks what a request asks about: which agents and how many days. It has
// already answered the request when ok is false. A creator only ever sees agents they created: an
// agent the platform provides, or someone else's, is not theirs to look at.
func (s *Server) activityScope(w http.ResponseWriter, r *http.Request) (ids []string, days int, ok bool) {
	if s.activity == nil {
		writeError(w, http.StatusNotImplemented, errors.New("activity is not available"))
		return nil, 0, false
	}
	days = 14
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			writeError(w, http.StatusBadRequest, errors.New("days must be a number from 1 to 30"))
			return nil, 0, false
		}
		days = n
	}
	user, signedIn := UserFromContext(r.Context())
	if !signedIn && s.authToken == "" {
		// Other creator routes run open when no key is configured (a dev default); how people are
		// doing is never open to anyone who asks.
		writeError(w, http.StatusUnauthorized, errUnauthorized)
		return nil, 0, false
	}
	mine := func(a *domain.Agent) bool { return !signedIn || (a.CreatedBy != "" && a.CreatedBy == user.ID) }

	if id := r.URL.Query().Get("agent_id"); id != "" {
		agent, err := s.store.LoadAgent(r.Context(), id)
		if err != nil || !mine(agent) {
			writeError(w, http.StatusNotFound, domain.ErrNotFound)
			return nil, 0, false
		}
		return []string{agent.ID}, days, true
	}
	var ownerID string
	if signedIn {
		ownerID = user.ID
	}
	list, err := s.store.ListAgents(r.Context(), ownerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return nil, 0, false
	}
	for _, a := range list {
		if mine(a) {
			ids = append(ids, a.ID)
		}
	}
	return ids, days, true
}

// getActivity returns the numbers for one of the creator's agents (?agent_id=) or for all of
// them, over ?days= (1 to 30, default 14).
func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	ids, days, ok := s.activityScope(w, r)
	if !ok {
		return
	}
	sum, err := s.activity.ActivitySummary(r.Context(), ids, days, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

const (
	// A stream asks again at least this often even when nothing was recorded, so numbers that fade
	// with time ("messages in the last 10 minutes") stay true.
	defaultActivityRefresh = 15 * time.Second
	// Changes that come close together are sent as one update.
	activityDebounce = 250 * time.Millisecond
	// A stream ends after this long and the screen reconnects: nothing stale is ever left open.
	maxActivityStream = 30 * time.Minute
	// How many streams one person, and the whole server, may hold open.
	maxStreamsPerUser = 4
	maxStreamsTotal   = 100
)

// WithActivityRefresh sets how often a stream asks again when nothing changed (for tests).
func WithActivityRefresh(d time.Duration) Option {
	return func(s *Server) { s.activityRefresh = d }
}

// streamSlots limits how many streams are open, so a screen left open in many tabs, or a client
// that never lets go, can't use up the server.
type streamSlots struct {
	mu    sync.Mutex
	total int
	byKey map[string]int
}

func (l *streamSlots) take(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		l.byKey = map[string]int{}
	}
	if l.total >= maxStreamsTotal || l.byKey[key] >= maxStreamsPerUser {
		return false
	}
	l.total++
	l.byKey[key]++
	return true
}

func (l *streamSlots) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.byKey[key]--; l.byKey[key] <= 0 {
		delete(l.byKey, key)
	}
}

// streamActivity pushes the same numbers as GET /activity as server-sent events: one right away,
// another a moment after anything is recorded for these agents, and one every so often anyway.
func (s *Server) streamActivity(w http.ResponseWriter, r *http.Request) {
	watcher, canWatch := s.activity.(domain.ActivityWatcher)
	ids, days, ok := s.activityScope(w, r)
	if !ok {
		return
	}
	if !canWatch {
		writeError(w, http.StatusNotImplemented, errors.New("live activity is not available"))
		return
	}
	rc := http.NewResponseController(w)
	key := "server"
	if user, signedIn := UserFromContext(r.Context()); signedIn {
		key = user.ID
	}
	if !s.streams.take(key) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many live views are open: close one and try again"))
		return
	}
	defer s.streams.release(key)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // proxies must not hold the stream back
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return // this connection can't stream: nothing more can be said
	}

	changed, stop := watcher.WatchActivity(ids)
	defer stop()

	send := func(event string, payload any) bool {
		raw, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	summary := func() bool {
		sum, err := s.activity.ActivitySummary(r.Context(), ids, days, time.Now())
		if err != nil {
			return r.Context().Err() == nil // a failed read is skipped; the next change or tick tries again
		}
		return send("summary", sum)
	}

	if !summary() {
		return
	}
	refresh := s.activityRefresh
	if refresh <= 0 {
		refresh = defaultActivityRefresh
	}
	tick := time.NewTicker(refresh)
	defer tick.Stop()
	end := time.NewTimer(maxActivityStream)
	defer end.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-end.C:
			send("bye", map[string]string{"reason": "reconnect"})
			return
		case <-tick.C:
			if !summary() {
				return
			}
		case <-changed:
			// Let changes that arrive together become one update.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(activityDebounce):
			}
			select {
			case <-changed:
			default:
			}
			if !summary() {
				return
			}
		}
	}
}
