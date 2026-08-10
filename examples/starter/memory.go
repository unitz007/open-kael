package main

import (
	"context"
	"sync"

	"github.com/unitz007/kael/llm"
)

// maxHistoryMessages bounds how many turns a memory.Memory implementation
// keeps here — a plain trim-the-oldest window, not summarization. Shared by
// both InMemoryHistory and FileHistory (memory_file.go) in this reference;
// pick whatever fits your own use case when you copy these.
const maxHistoryMessages = 20

// InMemoryHistory is a minimal memory.Memory implementation — process-local,
// lost on restart. Keyed by a plain conversation id (e.g.
// ConversationRef.ChatID) so separate conversations don't bleed into each
// other, independent of which platform they're on. Copy this file as the
// starting point for your own Memory (a database, Redis, whatever fits) —
// see memory_file.go for a JSON-file-backed variant that survives a restart.
type InMemoryHistory struct {
	mu   sync.Mutex
	byID map[string][]llm.Message
}

func NewInMemoryHistory() *InMemoryHistory {
	return &InMemoryHistory{byID: make(map[string][]llm.Message)}
}

func (m *InMemoryHistory) History(ctx context.Context, id string) []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.byID[id]
	out := make([]llm.Message, len(existing))
	copy(out, existing)
	return out
}

func (m *InMemoryHistory) Append(ctx context.Context, id string, messages ...llm.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	updated := append(m.byID[id], messages...)
	if len(updated) > maxHistoryMessages {
		updated = updated[len(updated)-maxHistoryMessages:]
	}
	m.byID[id] = updated
}
