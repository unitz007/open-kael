package runtime_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// funcLLM is a deterministic, scripted LLM — same pattern domain's own
// tests use (see domain/loop_test.go), no real provider needed.
type funcLLM struct {
	n    int
	call func(n int) (*domain.LLMResponse, error)
}

func (f *funcLLM) Call(_ context.Context, _ []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	f.n++
	return f.call(f.n)
}

// fakeExecutor is a minimal domain.Executor recording every call it
// receives — enough to assert on ActionSendMessage delivery or a gated
// tool actually running.
type fakeExecutor struct {
	calls []struct {
		action string
		input  map[string]any
	}
}

func (f *fakeExecutor) Execute(_ context.Context, _ *domain.Identity, _ string, action string, input map[string]any) (any, error) {
	f.calls = append(f.calls, struct {
		action string
		input  map[string]any
	}{action, input})
	return "ok", nil
}

// approvingExecutor additionally implements domain.ApprovalRequester,
// always resolving to approve — used to prove a RequiresApproval tool runs
// once approved.
type approvingExecutor struct {
	fakeExecutor
	approve bool
}

func (a *approvingExecutor) RequestApproval(_ context.Context, _ domain.ConversationRef, _ string, _ int) (bool, error) {
	return a.approve, nil
}

type memMemory struct {
	store map[string][]domain.Message
}

func newMemMemory() *memMemory { return &memMemory{store: make(map[string][]domain.Message)} }

func (m *memMemory) History(_ context.Context, id string) []domain.Message { return m.store[id] }
func (m *memMemory) Append(_ context.Context, id string, messages ...domain.Message) {
	m.store[id] = append(m.store[id], messages...)
}

func finishOnlyLLM(content string) *funcLLM {
	return &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
			{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": content}},
		}}, nil
	}}
}

// slackDeps builds a minimal AgentDeps wired for a "slack" conversation:
// a slack Integration and a matching Identity so resolveIdentityAndIntegration
// can find the executor for delivery/approval.
func slackDeps(extras ...func(*runtime.AgentDeps)) runtime.AgentDeps {
	slackIntegration := &domain.Integration{ID: "int_slack", Service: "slack"}
	slackIdentity := &domain.Identity{ID: "id_slack", IntegrationID: slackIntegration.ID}
	deps := runtime.AgentDeps{
		ToolsByID:        map[string]*domain.ToolDefinition{},
		IntegrationsByID: map[string]*domain.Integration{slackIntegration.ID: slackIntegration},
		IdentitiesByID:   map[string]*domain.Identity{slackIdentity.ID: slackIdentity},
	}
	for _, fn := range extras {
		fn(&deps)
	}
	return deps
}

func TestHandleTurn_DeliversFinalAnswerAndPersistsMemory(t *testing.T) {
	agent := &domain.Agent{ID: "a1", Instructions: "be helpful", LLMs: []domain.LLM{finishOnlyLLM("hello there")}, MaxIterations: 5}

	executors := domain.NewExecutorRegistry()
	slackExec := &fakeExecutor{}
	executors.Register("slack", slackExec)

	mem := newMemMemory()
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = mem
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}

	host := runtime.NewHost()
	host.Register(hosted)

	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1"}
	result, err := host.HandleTurn(context.Background(), hosted, conv, "hi")
	if err != nil {
		t.Fatalf("handle turn: %v", err)
	}
	if result.Status != domain.LoopStatusComplete {
		t.Fatalf("expected complete, got %s", result.Status)
	}

	if len(slackExec.calls) != 1 || slackExec.calls[0].action != domain.ActionSendMessage || slackExec.calls[0].input["text"] != "hello there" {
		t.Fatalf("expected one ActionSendMessage delivering the final answer, got %+v", slackExec.calls)
	}

	if history := mem.History(context.Background(), "slack:C1"); len(history) == 0 {
		t.Fatalf("expected memory to have been appended for this conversation")
	}
}

// requiresApprovalSkill builds a single-tool Skill whose one Tool requires
// approval, bound to the "tool" service/integration — separate from the
// "slack" conversation service, matching how a real approval-gated tool
// (e.g. place_trade) is a different Integration than the messenger the
// human is talking through.
func requiresApprovalSkill() (*domain.Skill, *domain.ToolDefinition, *domain.Integration) {
	integration := &domain.Integration{ID: "int-tool", Service: "tool"}
	tool := &domain.ToolDefinition{
		ID:                     "t1",
		Name:                   "do_thing",
		Description:            "Does a sensitive thing",
		IntegrationID:          integration.ID,
		Action:                 "do_thing",
		RequiresApproval:       true,
		ApprovalTimeoutSeconds: 60,
	}
	skill := &domain.Skill{
		ID:           "s1",
		Name:         "do_thing_skill",
		AgentID:      "a1",
		Instructions: "call do_thing",
		Tools:        []domain.ToolBinding{{ToolID: tool.ID}},
	}
	return skill, tool, integration
}

func TestHandleTurn_ApprovalGate_RefusesWithoutApprover(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "do_thing_skill", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "done"}}}}, nil
	}}

	skill, tool, toolIntegration := requiresApprovalSkill()
	agent := &domain.Agent{ID: "a1", Instructions: "be helpful", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}}

	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &fakeExecutor{}) // NOT an ApprovalRequester
	toolExec := &fakeExecutor{}
	executors.Register("tool", toolExec)

	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.ToolsByID = map[string]*domain.ToolDefinition{tool.ID: tool}
		d.IntegrationsByID[toolIntegration.ID] = toolIntegration
		d.Executors = executors
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)

	_, err := host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1"}, "please do the thing")
	if err != nil {
		t.Fatalf("handle turn: %v", err)
	}
	if len(toolExec.calls) != 0 {
		t.Fatalf("expected the gated tool NOT to run without an ApprovalRequester, but it ran: %+v", toolExec.calls)
	}
}

func TestHandleTurn_ApprovalGate_RunsOnceApproved(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "do_thing_skill", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "done"}}}}, nil
	}}

	skill, tool, toolIntegration := requiresApprovalSkill()
	agent := &domain.Agent{ID: "a1", Instructions: "be helpful", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}}

	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &approvingExecutor{approve: true}) // IS the approver
	toolExec := &fakeExecutor{}
	executors.Register("tool", toolExec)

	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.ToolsByID = map[string]*domain.ToolDefinition{tool.ID: tool}
		d.IntegrationsByID[toolIntegration.ID] = toolIntegration
		d.Executors = executors
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)

	_, err := host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1"}, "please do the thing")
	if err != nil {
		t.Fatalf("handle turn: %v", err)
	}
	if len(toolExec.calls) != 1 || toolExec.calls[0].action != "do_thing" {
		t.Fatalf("expected the gated tool to run once approved, got %+v", toolExec.calls)
	}
}
