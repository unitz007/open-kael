package llm

import "github.com/unitz007/kael/tools"

type Message struct {
	Role       string
	Content    string
	ToolCalls  []tools.ToolCall // set when the assistant is calling tools
	ToolCallID string           // set on a tool-result message, links back to the call
	Name       string           // optional: which tool produced this result

}

type Response struct {
	Status    string           `json:"status"`
	Error     error            `json:"error,omitempty"`
	ToolCalls []tools.ToolCall `json:"tool_calls,omitempty"`
	Content   string           `json:"content,omitempty"`
	// FinishReason and Reasoning are diagnostic-only, carried straight
	// through from the provider — e.g. a "length" finish reason with
	// content/tool_calls both empty means the model spent its whole token
	// budget on Reasoning (a separate "thinking" channel some models use)
	// and never got to actually answer.
	FinishReason string `json:"finish_reason,omitempty"`
	Reasoning    string `json:"reasoning,omitempty"`
}

type LLM interface {
	Call(messages []Message, tools []*tools.ToolSpec) (*Response, error)
}
