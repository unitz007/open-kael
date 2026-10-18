package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// A whole turn: the model calls a capability with a size over what the creator allows. The
// service never sees the call, the model is told why in plain words, and the person is answered.
// The same capability within the rule runs.
func TestCreatorRules_ApplyToAWholeTurn(t *testing.T) {
	trade := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	messenger := &outbox{}
	executors.Register("slack", messenger)
	executors.Register("trade", trade)

	integ := &domain.Integration{ID: "int_trade", Service: "trade"}
	tool := &domain.ToolDefinition{
		ID: "tool_trade", Name: "place_trade", IntegrationID: integ.ID, Action: "trade.place", Description: "Places a trade.",
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{
			"symbol": {Type: domain.SchemaTypeString}, "size": {Type: domain.SchemaTypeInteger},
		}, Required: []string{"symbol", "size"}},
	}
	skill := &domain.Skill{ID: "s1", Name: "place_trade", Description: "Place a trade", AgentID: "a1", Tools: []domain.ToolBinding{{ToolID: tool.ID}}}

	var told []string // what the model was told after each call
	script := []map[string]any{{"symbol": "EURUSD", "size": 500.0}, {"symbol": "EURUSD", "size": 50.0}}
	step := 0
	scripted := llmFunc(func(msgs []domain.Message) *domain.LLMResponse {
		last := msgs[len(msgs)-1]
		if last.Role == domain.RoleTool && last.Name == "place_trade" {
			told = append(told, last.Content)
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": "I told them."}}}}
		}
		args := script[step]
		step++
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "c", Name: "place_trade", Arguments: args}}}
	})

	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{scripted}, MaxIterations: 5, Skills: []*domain.Skill{skill},
		Rules: []domain.ToolRule{{ToolID: tool.ID, Input: "size", Kind: domain.RuleMax, Value: 100.0}}}
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
		d.ToolsByID[tool.ID] = tool
		d.IntegrationsByID[integ.ID] = integ
	})}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })

	say := func(text string) {
		host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
			Conversation: domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack"}, Text: text,
		})
	}
	say("buy 500 EURUSD")
	if len(trade.calls()) != 0 {
		t.Fatalf("a call over the creator's limit must never reach the service: %+v", trade.calls())
	}
	if len(told) != 1 || !strings.Contains(told[0], "more than the most allowed for size (100)") || !strings.Contains(told[0], "nothing was done") {
		t.Fatalf("the model is told why, in plain words: %q", told)
	}
	say("buy 50 EURUSD")
	if len(trade.calls()) != 1 {
		t.Fatalf("within the limit it runs: %d calls", len(trade.calls()))
	}
}

// llmFunc adapts a function to domain.LLM.
type llmFunc func(msgs []domain.Message) *domain.LLMResponse

func (f llmFunc) Call(_ context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	return f(msgs), nil
}
