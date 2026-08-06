// Package memory defines the Memory interface only — no implementations
// ship here, same reasoning as identity/, webhook/, and rag/. See
// examples/starter for a process-local (InMemoryHistory) and a
// file-persisted (FileHistory) reference implementation, both copyable.
package memory

import "github.com/unitz007/kael/llm"

// Memory holds an agent's conversation turns across separate inbound
// messages, keyed by an arbitrary string id. What id means is entirely up
// to the caller — nothing here assumes it's a conversation, a user, or
// anything else.
type Memory interface {
	History(id string) []llm.Message
	Append(id string, messages ...llm.Message)
}
