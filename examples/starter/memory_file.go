package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"

	"github.com/unitz007/kael/llm"
)

// FileHistory is a memory.Memory implementation backed by a JSON file on
// disk instead of a map that only lives as long as the process does — see
// memory.go's InMemoryHistory for the process-local counterpart. Every
// Append persists the full updated store, so conversation history survives
// a restart instead of starting over from nothing every time. Copy this
// file as the starting point for your own persistent Memory (swap the JSON
// file for a database, an object store, whatever fits).
type FileHistory struct {
	mu   sync.Mutex
	path string
	byID map[string][]llm.Message
}

// NewFileHistory loads whatever history already exists at path (if
// anything — a missing file just means starting empty, same as
// InMemoryHistory always does) and returns a FileHistory backed by it.
func NewFileHistory(path string) (*FileHistory, error) {
	f := &FileHistory{path: path, byID: make(map[string][]llm.Message)}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(data, &f.byID); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *FileHistory) History(ctx context.Context, id string) []llm.Message {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing := f.byID[id]
	out := make([]llm.Message, len(existing))
	copy(out, existing)
	return out
}

func (f *FileHistory) Append(ctx context.Context, id string, messages ...llm.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()

	updated := append(f.byID[id], messages...)
	if len(updated) > maxHistoryMessages {
		updated = updated[len(updated)-maxHistoryMessages:]
	}
	f.byID[id] = updated

	if err := f.persist(); err != nil {
		// Not fatal — the in-memory copy is still correct for the rest of
		// this process's lifetime, it just won't survive the next restart
		// until a write actually succeeds. Logged rather than silently
		// dropped, since a persistently-failing write here would otherwise
		// look identical to memory working fine.
		log.Printf("memory: failed to persist history to %s: %v", f.path, err)
	}
}

// persist writes the full store to a temp file and renames it into place —
// rename is atomic on the same filesystem, so a crash mid-write can never
// leave the real history file half-written or corrupt.
func (f *FileHistory) persist() error {
	data, err := json.MarshalIndent(f.byID, "", "  ")
	if err != nil {
		return err
	}

	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
