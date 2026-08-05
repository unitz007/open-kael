package memory

import (
	"sync"

	"github.com/unitz007/kael/llm"
)

// maxHistoryMessages bounds how many turns InMemoryHistory keeps — a plain
// trim-the-oldest window, not summarization. Good enough for a first cut;
// revisit if conversations regularly need more context than this holds.
const maxHistoryMessages = 20

// InMemoryHistory is the first real implementation of agent.Memory —
// process-local, lost on restart. Keyed by a plain conversation id (e.g.
// ConversationRef.ChatID) so separate conversations don't bleed into each
// other, independent of which platform they're on.
type InMemoryHistory struct {
	mu   sync.Mutex
	byID map[string][]llm.Message
}

func NewInMemoryHistory() *InMemoryHistory {
	return &InMemoryHistory{byID: make(map[string][]llm.Message)}
}

func (m *InMemoryHistory) History(id string) []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.byID[id]
	out := make([]llm.Message, len(existing))
	copy(out, existing)
	return out
}

func (m *InMemoryHistory) Append(id string, messages ...llm.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	updated := append(m.byID[id], messages...)
	if len(updated) > maxHistoryMessages {
		updated = updated[len(updated)-maxHistoryMessages:]
	}
	m.byID[id] = updated
}
