package postgres

import (
	"sync"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.ActivityWatcher = (*Store)(nil)

// activityHub passes "something changed" from the places that record activity to the screens
// watching it. It carries no data: a watcher asks for the numbers when told.
type activityHub struct {
	mu   sync.Mutex
	next int
	subs map[int]*activitySub
}

type activitySub struct {
	agents map[string]struct{}
	ch     chan struct{}
}

// WatchActivity implements domain.ActivityWatcher.
func (s *Store) WatchActivity(agentIDs []string) (<-chan struct{}, func()) {
	set := make(map[string]struct{}, len(agentIDs))
	for _, id := range agentIDs {
		set[id] = struct{}{}
	}
	sub := &activitySub{agents: set, ch: make(chan struct{}, 1)}
	h := &s.hub
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[int]*activitySub{}
	}
	h.next++
	id := h.next
	h.subs[id] = sub
	h.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
		})
	}
}

// notifyActivity tells everyone watching this agent that something changed. It never blocks: a
// watcher that has not yet read the last signal already knows.
func (s *Store) notifyActivity(agentID string) {
	h := &s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs {
		if _, ok := sub.agents[agentID]; !ok {
			continue
		}
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

func (s *Store) watchers() int {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return len(s.hub.subs)
}
