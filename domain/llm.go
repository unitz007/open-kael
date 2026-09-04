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
// One deviation: ctx is threaded through explicitly.
//
// Not used by the Sports Agent pilot — Sports agent stays on
// kael-platform's own llm.LLM/nativeLoop (see Kael/agents/domainbridge).
// Kept here for API completeness and for domain/'s own test suite.
type LLM interface {
	Call(ctx context.Context, messages []Message, actions []ActionSpec) (*LLMResponse, error)
}
