package runtime_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func TestChannelRedeemer_ValidCode_EmitsEventSkipsHandleTurn(t *testing.T) {
	// Skill subscribed to user.channel.connected — proves the event fires.
	skill, tool, toolIntegration := singleToolSkill("user.channel.connected", nil)

	toolExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration},
			Executors:        executors,
		},
	}

	host := runtime.NewHost()
	host.Register(hosted)
	host.RegisterEventSources(http.NewServeMux(), domain.NewEventSourceRegistry())

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

	// Skill triggered by user.channel.connected should have run.
	waitForCalls(t, toolExec, 1)

	if !redeemed {
		t.Fatal("expected channel redeemer to be called")
	}
	if toolExec.calls[0].input["trigger_input"] == nil {
		t.Fatalf("expected event payload in trigger_input, got %+v", toolExec.calls[0].input)
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
	skill, tool, toolIntegration := singleToolSkill("user.channel.connected", nil)
	toolExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration},
			Executors:        executors,
		},
	}

	host := runtime.NewHost()
	host.Register(hosted)
	host.RegisterEventSources(http.NewServeMux(), domain.NewEventSourceRegistry())

	var gotCode string
	host.SetChannelRedeemer(func(_ context.Context, code, _, _ string) (string, error) {
		gotCode = code
		return "user-42", nil
	})

	host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "telegram", ChatID: "chat-1", IdentityID: "id-tg"},
		Text:         "/start mydeeplinkcode123",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gotCode == "" {
		time.Sleep(5 * time.Millisecond)
	}
	if gotCode != "mydeeplinkcode123" {
		t.Fatalf("expected code %q from /start command, got %q", "mydeeplinkcode123", gotCode)
	}
}

func TestChannelRedeemer_PlainSentence_NotAttempted(t *testing.T) {
	chatIntegration := &domain.Integration{ID: "int-chat", Service: "chat"}
	chatTool := &domain.ToolDefinition{
		ID: "chat-tool", Name: "reply", IntegrationID: chatIntegration.ID, Action: "reply",
	}
	chatSkill := &domain.Skill{
		ID: "s-chat", Name: "chat_skill", AgentID: "a1",
		Tools: []domain.ToolBinding{{ToolID: chatTool.ID}},
	}

	chatExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("chat", chatExec)

	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
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

	called := false
	host := runtime.NewHost()
	host.Register(hosted)
	host.RegisterEventSources(http.NewServeMux(), domain.NewEventSourceRegistry())
	host.SetChannelRedeemer(func(_ context.Context, _, _, _ string) (string, error) {
		called = true
		return "", domain.ErrNotFound
	})

	// A sentence with spaces should never even reach the redeemer — extractLinkCode returns "".
	// UserID is set so the host treats this as a linked user and runs HandleTurn.
	host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "slack", ChatID: "C123", IdentityID: "id-sl", UserID: "user-42"},
		Text:         "hello how are you",
	})

	waitForCalls(t, chatExec, 1)
	if called {
		t.Fatal("expected redeemer NOT to be called for a plain sentence with spaces")
	}
}
