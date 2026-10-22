package runtime

import (
	"sort"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.WorkWatcher = (*Host)(nil)

// workBoard counts, for each agent, the turns it is in the middle of right now, and tells whoever is watching when an
// agent starts or stops working. It is only about this moment: nothing is kept.
type workBoard struct {
	mu   sync.Mutex
	n    map[string]int
	subs map[int]*workSub
	next int
}

type workSub struct {
	agents map[string]struct{}
	ch     chan struct{}
}

// beginWork marks an agent as working and returns the function that marks that piece of work done. Watchers are told
// only when the agent goes from idle to working or back, not for every overlapping turn.
func (h *Host) beginWork(agentID string) (done func()) {
	b := &h.work
	b.mu.Lock()
	if b.n == nil {
		b.n = map[string]int{}
	}
	b.n[agentID]++
	started := b.n[agentID] == 1
	b.mu.Unlock()
	if started {
		b.notify(agentID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.n[agentID]--
			stopped := b.n[agentID] <= 0
			if stopped {
				delete(b.n, agentID)
			}
			b.mu.Unlock()
			if stopped {
				b.notify(agentID)
			}
		})
	}
}

func (b *workBoard) notify(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		if _, ok := sub.agents[agentID]; !ok {
			continue
		}
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
}

// Working implements domain.WorkWatcher.
func (h *Host) Working(agentIDs []string) []string {
	b := &h.work
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for _, id := range agentIDs {
		if b.n[id] > 0 {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// WatchWork implements domain.WorkWatcher.
func (h *Host) WatchWork(agentIDs []string) (<-chan struct{}, func()) {
	set := make(map[string]struct{}, len(agentIDs))
	for _, id := range agentIDs {
		set[id] = struct{}{}
	}
	sub := &workSub{agents: set, ch: make(chan struct{}, 1)}
	b := &h.work
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[int]*workSub{}
	}
	b.next++
	id := b.next
	b.subs[id] = sub
	b.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
		})
	}
}
