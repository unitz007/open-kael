package runtime_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func TestChannelRedeemer_ValidCode_SkipsHandleTurn(t *testing.T) {
	// The agent has no model and no abilities: a turn would have nothing to run, so a redeemed code never reaches one.
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1"}, Deps: runtime.AgentDeps{Executors: domain.NewExecutorRegistry()}}
	host := runtime.NewHost()
	host.Register(hosted)

	redeemed := false
	host.SetChannelRedeemer(func(_ context.Context, code, _, _ string) (string, error) {
		if code == "validlinkcode1234" {
			redeemed = true
			return "user-42", nil
		}
		return "", domain.ErrNotFound
	})

	host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "telegram", ChatID: "chat-1", IdentityID: "id-tg"},
		Text:         "validlinkcode1234",
	})
	if !redeemed {
		t.Fatal("expected channel redeemer to be called")
	}
}

func TestChannelRedeemer_InvalidCode_FallsThroughToHandleTurn(t *testing.T) {
	chatIntegration := &domain.Integration{ID: "int-chat", Service: "chat"}
	chatTool := &domain.ToolDefinition{
		ID: "chat-tool", Name: "reply", IntegrationID: chatIntegration.ID, Action: "reply",
	}
	chatSkill := &domain.Skill{
		ID:      "s-chat",
		Name:    "chat_skill",
		AgentID: "a1",
		Tools:   []domain.ToolBinding{{ToolID: chatTool.ID}},
	}

	chatExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("chat", chatExec)

	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			// BindSkill exposes the skill under skill.Name, not the underlying tool name.
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{Name: "chat_skill", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{Name: domain.FinishActionName, Arguments: map[string]any{"content": "done"}}}}, nil
	}}

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{chatSkill}, LLMs: []domain.LLM{llm}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{chatTool.ID: chatTool},
			IntegrationsByID: map[string]*domain.Integration{chatIntegration.ID: chatIntegration},
			Executors:        executors,
		},
	}

	host := runtime.NewHost()
	host.Register(hosted)
	host.RegisterEventSources(http.NewServeMux(), domain.NewEventSourceRegistry())
	host.SetChannelRedeemer(func(_ context.Context, _, _, _ string) (string, error) {
		return "", domain.ErrNotFound
	})

	// UserID must be set so the host treats this as a linked user and runs
	// HandleTurn rather than sending an onboarding message.
	host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "telegram", ChatID: "chat-1", IdentityID: "id-tg", UserID: "user-42"},
		Text:         "notacode12345678",
	})

	waitForCalls(t, chatExec, 1)
}

func TestChannelRedeemer_TelegramStartCommand_ExtractsCode(t *testing.T) {
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1"}, Deps: runtime.AgentDeps{Executors: domain.NewExecutorRegistry()}}
	host := runtime.NewHost()
	host.Register(hosted)

	var gotCode string
	host.SetChannelRedeemer(func(_ context.Context, code, _, _ string) (string, error) {
		gotCode = code
		return "user-42", nil
	})

	host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "telegram", ChatID: "chat-1", IdentityID: "id-tg"},
		Text:         "/start mydeeplinkcode123",
	})
	if gotCode != "mydeeplinkcode123" {
		t.Fatalf("expected the deep-link code to be extracted, got %q", gotCode)
	}
}
