// Package memory defines the Memory interface only — no implementations
// ship here, same reasoning as identity/, webhook/, and rag/. See
// examples/starter for a process-local (InMemoryHistory) and a
// file-persisted (FileHistory) reference implementation, both copyable.
package memory

import (
	"context"

	"github.com/unitz007/kael/llm"
)

// Memory holds an agent's conversation turns across separate inbound
// messages, keyed by an arbitrary string id. What id means is entirely up
// to the caller — nothing here assumes it's a conversation, a user, or
// anything else. ctx carries whatever the current run already has attached
// (e.g. messaging.ConversationFromContext) — an implementation that has no
// use for it is free to ignore it, same as any other ctx-accepting method
// in this codebase.
type Memory interface {
	History(ctx context.Context, id string) []llm.Message
	Append(ctx context.Context, id string, messages ...llm.Message)
}
