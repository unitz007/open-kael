package domain

import "context"

type LLMResponse struct {
	Content   string
	ToolCalls []ToolCall
	Reasoning string
}

// LLM mirrors kael-platform/llm.LLM's shape (blocking Call, actions passed
// alongside messages, tool-calls-vs-final-answer told apart by whether
// ToolCalls is empty) — a deliberate borrow of a proven-simple contract.
// One deviation: ctx is threaded through explicitly, since the old
// interface's lack of one was a gap, not a feature worth preserving.
type LLM interface {
	Call(ctx context.Context, messages []Message, actions []ActionSpec) (*LLMResponse, error)
}

// StreamChunk is one piece of a streaming LLM response. Content holds the
// token delta; Done is true on the final sentinel (Content may be empty).
// Concatenating all non-sentinel Content fields reconstructs the full reply.
type StreamChunk struct {
	Content string
	Done    bool
}

// StreamingLLM is an optional interface that LLM implementations may satisfy
// to support token-level streaming. The returned channel carries deltas until
// a Done sentinel; the caller must drain it even on ctx cancellation.
// The runtime prefers StreamCall over Call for the final synthesis step when
// both an executor StreamingMessenger and a StreamingLLM are available.
type StreamingLLM interface {
	LLM
	StreamCall(ctx context.Context, messages []Message, actions []ActionSpec) (<-chan StreamChunk, error)
}
