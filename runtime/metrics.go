package runtime

import (
	"sort"
	"sync"
)

// Metrics counts what happens to deliveries, by source. It is cheap and safe to use from many goroutines.
type Metrics struct {
	mu     sync.Mutex
	counts map[string]map[string]int64
}

func NewMetrics() *Metrics { return &Metrics{counts: map[string]map[string]int64{}} }

// Inc adds one to a counter of a source (received, rejected, unknown, ignored, duplicate, stored, forwarded, throttled, error).
func (m *Metrics) Inc(source, name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts[source] == nil {
		m.counts[source] = map[string]int64{}
	}
	m.counts[source][name]++
}

// Snapshot is a copy of every counter, by source.
func (m *Metrics) Snapshot() map[string]map[string]int64 {
	out := map[string]map[string]int64{}
	if m == nil {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for src, c := range m.counts {
		cp := make(map[string]int64, len(c))
		for k, v := range c {
			cp[k] = v
		}
		out[src] = cp
	}
	return out
}

// Sources lists the sources that have counters, sorted.
func (m *Metrics) Sources() []string {
	snap := m.Snapshot()
	out := make([]string, 0, len(snap))
	for s := range snap {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
