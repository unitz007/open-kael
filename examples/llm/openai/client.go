// Package openai is a working, copyable reference implementation of
// llm.LLM, talking to any OpenAI-compatible chat-completions endpoint
// (OpenRouter, OpenAI itself, or any self-hosted equivalent) — same
// reasoning as examples/messenger: the core library ships no LLM
// implementations of its own, the interface (llm.LLM) is the contract.
// Copy this package into your own project, rename it, and edit freely —
// it isn't meant to be depended on as a permanent import from here. In
// particular, if you need to pin/exclude specific providers (e.g. via
// OpenRouter's provider routing), that's exactly the kind of deployment-
// specific tuning that belongs in your own copy, not upstream here.
package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/tools"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// httpClient is used for every chat-completions call instead of
// http.DefaultClient, which has no Timeout at all — a stalled connection or
// a hung provider would otherwise block Call (and with it, the single
// inbox-processing goroutine, and every message queued behind it) forever,
// with no error and nothing for any of the loop's own guards to react to,
// since none of them run until Call actually returns. Was 120s, but
// production logs showed real requests — full conversation history plus
// every registered tool's schema, against a reasoning model that "thinks"
// before answering — legitimately exceeding that under normal load, not just
// during a hang. 240s keeps the same hang-detection guarantee while giving
// genuinely slow (not stuck) turns room to complete.
var httpClient = &http.Client{Timeout: 240 * time.Second}

type Role string
type ToolType string

const (
	RoleSystem   Role = "system"
	RoleUser     Role = "user"
	RoleAgent    Role = "assistant"
	RoleToolCall Role = "tool"

	ToolTypeFunction ToolType = "function"
)

type client struct {
	model           string
	apiKey          string
	baseUrl         string
	enableReasoning bool
}

// NewClient builds an OpenRouter-compatible LLM client bound to one model.
// enableReasoning controls whether that model's separate reasoning/"thinking"
// channel is requested — a reasoning-capable model has been observed
// dropping the handoff from its reasoning to its actual tool_calls entirely
// (finish_reason "stop", both content and tool_calls empty, even though the
// reasoning text correctly named the tool to call). Per-agent since it's a
// property of the model/task pairing, not something every agent using this
// client necessarily wants disabled — pass false unless a given agent's use
// case actually benefits from seeing the model's reasoning trace.
func NewClient(model string, enableReasoning bool) llm.LLM {

	apiKey, ok := os.LookupEnv("LLM_API_KEY")
	if !ok {
		slog.Error("LLM_API_KEY is required")
		os.Exit(1)
	}

	baseUrl, ok := os.LookupEnv("LLM_BASE_URL")
	if !ok {
		slog.Error("LLM_BASE_URL is required")
		os.Exit(1)
	}

	return client{
		model:           model,
		apiKey:          apiKey,
		baseUrl:         baseUrl,
		enableReasoning: enableReasoning,
	}
}

func (l client) Call(messages []llm.Message, t []*tools.ToolSpec) (*llm.Response, error) {
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + l.apiKey,
	}

	llmRequest := NewRequest(l.model, messages, t, l.enableReasoning)

	bodyJson, _ := json.Marshal(llmRequest)

	request, err := http.NewRequest(http.MethodPost, l.baseUrl+"/api/v1/chat/completions", bytes.NewReader(bodyJson))
	if err != nil {
		return nil, err
	}

	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}

	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			return
		}
	}(response.Body)

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		log.Printf("OpenRouter API returned status %d: %s", response.StatusCode, string(body))
		return nil, fmt.Errorf("OpenRouter API returned status %d: %s", response.StatusCode, string(body))
	}

	var responseBody Response
	err = json.NewDecoder(response.Body).Decode(&responseBody)
	if err != nil {
		log.Println("Error decoding OpenRouter API response:", err)
		return nil, err
	}

	toolCalls := make([]tools.ToolCall, 0)

	if len(responseBody.Choices) > 0 {
		for _, r := range responseBody.Choices {
			for _, t := range r.Message.ToolCalls {
				toolCalls = append(toolCalls, tools.ToolCall{
					Id:        t.Id,
					Type:      t.Type,
					Name:      t.Function.Name,
					Arguments: t.Function.Arguments,
				})
			}
		}
	}

	// Logged on every call (not just failures) so a decoding-glitch batch —
	// hundreds of near-identical tool_calls in one response, seen from
	// z-ai/glm-5.2 on tool-heavy requests — can be correlated to the specific
	// OpenRouter backend that served it, without waiting to reproduce it
	// again under a debugger.
	finishReason := ""
	if len(responseBody.Choices) > 0 {
		finishReason = responseBody.Choices[0].FinishReason
	}
	log.Printf("OpenRouter response: model=%s provider=%s finish_reason=%s tool_calls=%d",
		l.model, responseBody.Provider, finishReason, len(toolCalls))

	return &llm.Response{
		Content:      responseBody.Choices[0].Message.Content,
		ToolCalls:    toolCalls,
		FinishReason: responseBody.Choices[0].FinishReason,
		Reasoning:    responseBody.Choices[0].Message.Reasoning,
	}, nil
}
