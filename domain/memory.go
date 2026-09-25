package domain

import "context"

// Memory holds an Agent's conversation turns across separate inbound
// messages, keyed by an arbitrary string id — what id means (a
// conversation, a user, a thread) is entirely up to the caller. No
// implementation ships here, same reasoning as Executor: a concrete store
// (Postgres, in-process, file-backed) belongs to whoever embeds this
// framework, not the framework itself.
type Memory interface {
	History(ctx context.Context, id string) []Message
	Append(ctx context.Context, id string, messages ...Message)
}
