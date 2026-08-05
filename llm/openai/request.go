package openai

import (
	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/tools"
	"encoding/json"
)

type ToolSchema struct {
	Type     string          `json:"type"`
	Function FunctionRequest `json:"function"`
}

type FunctionRequest struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Parameters  Parameter `json:"parameters"`
}

type Parameter struct {
	Type                 string              `json:"type"`
	Properties           map[string]Property `json:"properties"`
	Required             []string            `json:"required,omitempty"`
	AdditionalProperties bool                `json:"additionalProperties"`
}

type Property struct {
	Type        string    `json:"type"`
	Description string    `json:"description,omitempty"`
	Items       *Property `json:"items,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
}

type Request struct {
	Model       string              `json:"model"`
	Message     []Message           `json:"messages"`
	Tools       []ToolSchema        `json:"tools"`
	ToolChoice  string              `json:"tool_choice,omitempty"`
	Reasoning   *ReasoningConfig    `json:"reasoning,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature"`
	Provider    *ProviderPreference `json:"provider,omitempty"`
}

// ProviderPreference is OpenRouter's request-level routing control.
// badProviders excludes backends confirmed (via the provider logging added
// alongside this) to have a decoding bug on z-ai/glm-5.2 tool-calling
// requests: DeepInfra and Venice returned hundreds of near-identical
// tool_calls in a single response on every observed call — a 100% failure
// rate — while StreamLake served the exact same requests cleanly every
// time. This isn't the model or this codebase's request shape; it's those
// two backends specifically.
type ProviderPreference struct {
	Ignore []string `json:"ignore,omitempty"`
}

var badProviders = []string{"DeepInfra", "Venice"}

// defaultMaxTokens caps a single call's completion length. Without this, a
// reasoning-capable model (e.g. z-ai/glm-5.2) generates an unbounded
// chain-of-thought before answering — observed taking 240s+ on a tool-heavy
// request (many registered tools, full conversation history) even with
// enableReasoning=false, since that flag only controls whether the reasoning
// trace is returned, not whether the model reasons at all. Raising the HTTP
// client timeout alone doesn't fix this: an uncapped generation just eats
// whatever timeout it's given. 4096 is generous for a single tool-call
// decision while guaranteeing the call can't run away indefinitely.
const defaultMaxTokens = 4096

// ReasoningConfig toggles a reasoning/"thinking" model's separate reasoning
// channel (an OpenRouter-level request field) — see NewClient's
// enableReasoning for why this needs to be configurable rather than always
// on or off. Every call in this codebase runs with ToolChoice "required",
// and a model that writes its plan into a separate reasoning field before
// its actual tool_calls has been observed dropping the handoff entirely —
// finish_reason "stop" with both content and tool_calls empty, even though
// reasoning correctly named the right tool to call. Disabling it worked
// around that for the agent it was observed on.
type ReasoningConfig struct {
	Enabled bool `json:"enabled"`
}

type Message struct {
	Role       Role              `json:"role"`
	ToolCalls  []ToolCallRequest `json:"tool_calls,omitempty"`
	Content    any               `json:"content"`
	ToolCallId string            `json:"tool_call_id,omitempty"`
}

// ToolCallRequest is the wire shape a tool call must take when it's echoed
// back on an assistant message (replaying conversation history) — nested
// under "function", not the flattened shape tools.ToolCall uses internally
// for a *parsed* response. Sending the flat shape here is silently accepted
// by the API's schema but leaves the message with no tool_calls the API can
// recognize, so any following role:"tool" message gets rejected as
// orphaned.
type ToolCallRequest struct {
	Id       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func NewRequest(model string, messages []llm.Message, tR []*tools.ToolSpec, enableReasoning bool) *Request {

	schemas := make([]ToolSchema, 0, len(tR))

	for _, ts := range tR {
		parameters := make(map[string]Property)
		required := make([]string, 0)

		for _, param := range ts.Parameters {
			parameters[param.Name] = Property{
				Type:        param.Type,
				Description: param.Description,
			}
			if param.Required {
				required = append(required, param.Name)
			}
		}

		schemas = append(schemas, ToolSchema{
			Type: "function",
			Function: FunctionRequest{
				Name:        ts.Name,
				Description: ts.Description,
				Parameters: Parameter{
					Type:                 "object",
					Properties:           parameters,
					Required:             required,
					AdditionalProperties: false,
				},
			},
		})
	}

	return &Request{
		Model:       model,
		ToolChoice:  "required",
		Reasoning:   &ReasoningConfig{Enabled: enableReasoning},
		MaxTokens:   defaultMaxTokens,
		Temperature: 0, // deterministic decoding — see client.go's provider logging for why
		Provider:    &ProviderPreference{Ignore: badProviders},
		Message: func() []Message {
			m := make([]Message, 0, len(messages))
			for _, message := range messages {
				toolCalls := make([]ToolCallRequest, 0, len(message.ToolCalls))
				for _, tc := range message.ToolCalls {
					toolCalls = append(toolCalls, ToolCallRequest{
						Id:   tc.Id,
						Type: tc.Type,
						Function: ToolCallFunction{
							Name:      tc.Name,
							Arguments: tc.Arguments,
						},
					})
				}

				m = append(m, Message{
					Role:       Role(message.Role),
					Content:    message.Content,
					ToolCalls:  toolCalls,
					ToolCallId: message.ToolCallID,
				})
			}

			return m
		}(),
		Tools: func() []ToolSchema {
			ts := make([]ToolSchema, 0, len(schemas))
			for _, schema := range schemas {
				ts = append(ts, schema)
			}
			return ts
		}(),
	}
}
