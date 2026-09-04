package domain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/kael/domain"
)

func TestAgentRegistry_PublicSkillsReachableAcrossAgents(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	tool := &domain.ToolDefinition{ID: "tool_1", Name: "echo", IntegrationID: integration.ID, Action: "mock.echo"}

	publicSkill := &domain.Skill{
		ID: "skill_public", Name: "public_skill", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: tool.ID}},
	}
	privateSkill := &domain.Skill{
		ID: "skill_private", Name: "private_skill", Visibility: domain.SkillPrivate,
		Tools: []domain.ToolBinding{{ToolID: tool.ID}},
	}

	agentB := &domain.Agent{ID: "agent_b", Skills: []*domain.Skill{publicSkill, privateSkill}}
	agentA := &domain.Agent{ID: "agent_a"}

	er := domain.NewExecutorRegistry()
	er.Register("mock", echoExecutor{})
	registry := domain.NewInMemoryRegistry(er)
	registry.RegisterIntegration(integration)
	registry.RegisterTool(tool)
	registry.RegisterAgent(agentB)
	registry.RegisterAgent(agentA)

	actions := agentA.Directory.PublicSkills(context.Background())
	if len(actions) != 1 {
		t.Fatalf("expected 1 reachable public skill, got %d", len(actions))
	}
	if actions[0].Spec.Name != "public_skill" {
		t.Fatalf("unexpected action: %+v", actions[0].Spec)
	}

	bActions := agentB.Directory.PublicSkills(context.Background())
	if len(bActions) != 0 {
		t.Fatalf("expected agent B's own directory to exclude its own skills, got %d", len(bActions))
	}
}

func TestAgentRegistry_SkipsSkillWithUnresolvedTool(t *testing.T) {
	skill := &domain.Skill{
		ID: "skill_x", Name: "x", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: "missing_tool"}},
	}
	agentB := &domain.Agent{ID: "agent_b", Skills: []*domain.Skill{skill}}
	agentA := &domain.Agent{ID: "agent_a"}

	registry := domain.NewInMemoryRegistry(domain.NewExecutorRegistry())
	registry.RegisterAgent(agentB)
	registry.RegisterAgent(agentA)

	actions := agentA.Directory.PublicSkills(context.Background())
	if len(actions) != 0 {
		t.Fatalf("expected unresolved skill to be skipped, got %d actions", len(actions))
	}
}

func TestAgentRegistry_DelegateRespectsDepthLimit(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	tool := &domain.ToolDefinition{ID: "tool_1", Name: "echo", IntegrationID: integration.ID, Action: "mock.echo"}
	skill := &domain.Skill{
		ID: "skill_pub", Name: "pub", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: tool.ID}},
	}

	agentB := &domain.Agent{ID: "agent_b", Skills: []*domain.Skill{skill}}
	agentA := &domain.Agent{ID: "agent_a"}

	er := domain.NewExecutorRegistry()
	er.Register("mock", echoExecutor{})
	registry := domain.NewInMemoryRegistry(er)
	registry.RegisterIntegration(integration)
	registry.RegisterTool(tool)
	registry.RegisterAgent(agentB)
	registry.RegisterAgent(agentA)

	actions := agentA.Directory.PublicSkills(context.Background())
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}

	ctx := context.Background()
	for i := 0; i < domain.MaxDelegationDepth; i++ {
		ctx, _, _ = domain.IncrementDelegationDepth(ctx)
	}
	if _, err := actions[0].Invoke(ctx, map[string]any{}); err == nil {
		t.Fatal("expected an error once delegation depth is exceeded")
	}
}

// TestAgentRegistry_FullDelegationThroughNativeLoop is the registry-backed
// counterpart to loop_test.go's TestDelegation_CallsTargetAgentsOwnLoop —
// same chain, but agentA.Directory now comes from a real AgentRegistry
// instead of a hand-rolled staticDirectory.
func TestAgentRegistry_FullDelegationThroughNativeLoop(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	toolRepo := &domain.ToolDefinition{ID: "tool_repo", Name: "lookup_repo", IntegrationID: integration.ID, Action: "mock.lookup_repo"}
	toolBranch := &domain.ToolDefinition{ID: "tool_branch", Name: "lookup_branch", IntegrationID: integration.ID, Action: "mock.lookup_branch"}

	skillB := &domain.Skill{
		ID: "skill_ship_pr", Name: "ship_pull_request", Visibility: domain.SkillPublic,
		Instructions: "Look up the repo and branch, then finish with the PR url.",
		OutputSchema: domain.Schema{
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"url": {Type: domain.SchemaTypeString}},
		},
		Tools: []domain.ToolBinding{{ToolID: toolRepo.ID}, {ToolID: toolBranch.ID}},
	}

	llmB := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		switch n {
		case 1:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "lookup_repo", Arguments: map[string]any{}}}}, nil
		case 2:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: "lookup_branch", Arguments: map[string]any{}}}}, nil
		default:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "3", Name: domain.FinishActionName, Arguments: map[string]any{"url": "https://example.com/pr/1"}}}}, nil
		}
	}}
	agentB := &domain.Agent{ID: "agent_b", Name: "Agent B", LLMs: []domain.LLM{llmB}, MaxIterations: 5, Skills: []*domain.Skill{skillB}}

	llmA := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "ship_pull_request", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "done via registry"}}}}, nil
	}}
	agentA := &domain.Agent{ID: "agent_a", Name: "Agent A", LLMs: []domain.LLM{llmA}, MaxIterations: 5}

	er := domain.NewExecutorRegistry()
	er.Register("mock", echoExecutor{})
	registry := domain.NewInMemoryRegistry(er)
	registry.RegisterIntegration(integration)
	registry.RegisterTool(toolRepo)
	registry.RegisterTool(toolBranch)
	registry.RegisterAgent(agentB)
	registry.RegisterAgent(agentA)

	loopA := domain.NewNativeLoop(agentA.LLMs, agentA.MaxIterations)
	result, transcript, err := loopA.Run(context.Background(), nil, agentA.Directory.PublicSkills(context.Background()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != domain.LoopStatusComplete {
		t.Fatalf("expected LoopStatusComplete, got %s (%s)", result.Status, result.Content)
	}
	if result.Content != "done via registry" {
		t.Fatalf("unexpected content: %q", result.Content)
	}

	var sawDelegatedResult bool
	for _, m := range transcript {
		if m.Role == domain.RoleTool && m.Name == "ship_pull_request" && strings.Contains(m.Content, "https://example.com/pr/1") {
			sawDelegatedResult = true
		}
	}
	if !sawDelegatedResult {
		t.Fatalf("expected agent A's transcript to contain agent B's typed result, got %+v", transcript)
	}
}
