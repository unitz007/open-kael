package domain_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/unitz007/kael/domain"
)

// funcLLM is a deterministic, scripted LLM — no real provider needed to
// exercise NativeLoop. call is invoked once per Call, numbered from 1.
type funcLLM struct {
	n    int
	call func(n int) (*domain.LLMResponse, error)
}

func (f *funcLLM) Call(_ context.Context, _ []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	f.n++
	return f.call(f.n)
}

// TestNativeLoop_CompletesOnFinish covers a full loop over one BoundAction
// ending in an explicit "finish" call.
func TestNativeLoop_CompletesOnFinish(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "1", Name: "greet", Arguments: map[string]any{"name": "world"}},
			}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
			{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "done greeting"}},
		}}, nil
	}}

	greet := &domain.BoundAction{
		Spec: domain.ActionSpec{Name: "greet", Description: "Greets someone"},
		Invoke: func(_ context.Context, input map[string]any) (any, error) {
			return fmt.Sprintf("hello, %v", input["name"]), nil
		},
	}

	loop := domain.NewNativeLoop([]domain.LLM{llm}, 5)
	result, transcript, err := loop.Run(context.Background(), nil, []*domain.BoundAction{greet})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != domain.LoopStatusComplete {
		t.Fatalf("expected LoopStatusComplete, got %s", result.Status)
	}
	if result.Content != "done greeting" {
		t.Fatalf("expected content %q, got %q", "done greeting", result.Content)
	}

	var sawToolResult bool
	for _, m := range transcript {
		if m.Role == domain.RoleTool && m.Name == "greet" && strings.Contains(m.Content, "hello, world") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("expected transcript to contain greet's tool result, got %+v", transcript)
	}
}

// TestNativeLoop_MaxIterationExhausted covers a loop that never calls
// "finish" and instead keeps making distinct, successful action calls until
// the iteration cap is hit.
func TestNativeLoop_MaxIterationExhausted(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
			{ID: fmt.Sprint(n), Name: "step", Arguments: map[string]any{"i": n}},
		}}, nil
	}}

	step := &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: "step"},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) { return "ok", nil },
	}

	loop := domain.NewNativeLoop([]domain.LLM{llm}, 3)
	result, _, err := loop.Run(context.Background(), nil, []*domain.BoundAction{step})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != domain.LoopStatusMaxIteration {
		t.Fatalf("expected LoopStatusMaxIteration, got %s (%s)", result.Status, result.Content)
	}
}

// TestBindSkill_NestedLoopProducesTypedOutput covers a Skill with more than
// one bound Tool: BindSkill wraps it in a nested Loop whose "finish" call
// lands in LoopResult.Output, matching the Skill's OutputSchema.
func TestBindSkill_NestedLoopProducesTypedOutput(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		switch n {
		case 1:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "1", Name: "lookup_repo", Arguments: map[string]any{}},
			}}, nil
		case 2:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "2", Name: "lookup_branch", Arguments: map[string]any{}},
			}}, nil
		default:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "3", Name: domain.FinishActionName, Arguments: map[string]any{"url": "https://example.com/pr/1"}},
			}}, nil
		}
	}}

	lookupRepo := &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: "lookup_repo"},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) { return "kael", nil },
	}
	lookupBranch := &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: "lookup_branch"},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) { return "main", nil },
	}

	skill := &domain.Skill{
		ID:           "skill_ship_pr",
		Name:         "ship_pull_request",
		Description:  "Opens a pull request",
		Instructions: "Look up the repo and branch, then open a PR and finish with its URL.",
		OutputSchema: domain.Schema{
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"url": {Type: domain.SchemaTypeString}},
		},
		Tools: []domain.ToolBinding{{ToolID: "tool_lookup_repo"}, {ToolID: "tool_lookup_branch"}},
	}

	agent := &domain.Agent{ID: "agent_dev", LLMs: []domain.LLM{llm}, MaxIterations: 5}

	bound := domain.BindSkill(agent, skill, []*domain.BoundAction{lookupRepo, lookupBranch})
	result, err := bound.Invoke(context.Background(), map[string]any{"change": "add domain model"})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}

	output, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any output, got %T", result)
	}
	if output["url"] != "https://example.com/pr/1" {
		t.Fatalf("unexpected output: %+v", output)
	}
}

// staticDirectory is a minimal, test-local AgentDirectory — always returns
// the same fixed set of delegate actions.
type staticDirectory struct {
	actions []*domain.BoundAction
}

func (d *staticDirectory) PublicSkills(_ context.Context) []*domain.BoundAction {
	return d.actions
}

// TestDelegation_CallsTargetAgentsOwnLoop covers the full delegation chain:
// Agent A's top-level loop calls a delegate action from its Directory,
// which is wired (via BindSkill) to Agent B's own nested Loop — proving a
// typed result flows back into Agent A's transcript without any network or
// process boundary.
func TestDelegation_CallsTargetAgentsOwnLoop(t *testing.T) {
	// Agent B: the delegate target, with a two-tool Skill of its own.
	llmB := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		switch n {
		case 1:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "1", Name: "lookup_repo", Arguments: map[string]any{}},
			}}, nil
		case 2:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "2", Name: "lookup_branch", Arguments: map[string]any{}},
			}}, nil
		default:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "3", Name: domain.FinishActionName, Arguments: map[string]any{"url": "https://example.com/pr/1"}},
			}}, nil
		}
	}}
	lookupRepo := &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: "lookup_repo"},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) { return "kael", nil },
	}
	lookupBranch := &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: "lookup_branch"},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) { return "main", nil },
	}
	skillB := &domain.Skill{
		ID:           "skill_ship_pr",
		Name:         "ship_pull_request",
		Description:  "Opens a pull request",
		Instructions: "Look up the repo and branch, then open a PR and finish with its URL.",
		Visibility:   domain.SkillPublic,
		OutputSchema: domain.Schema{
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"url": {Type: domain.SchemaTypeString}},
		},
	}
	agentB := &domain.Agent{ID: "agent_b", Name: "Agent B", LLMs: []domain.LLM{llmB}, MaxIterations: 5}
	delegateAction := domain.BindSkill(agentB, skillB, []*domain.BoundAction{lookupRepo, lookupBranch})

	// Wrap with depth-limiting, as a real AgentDirectory implementation would.
	wrapped := &domain.BoundAction{
		Spec: delegateAction.Spec,
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			newCtx, depth, ok := domain.IncrementDelegationDepth(ctx)
			if !ok {
				return nil, fmt.Errorf("delegation depth %d exceeds max", depth)
			}
			return delegateAction.Invoke(newCtx, input)
		},
	}
	directory := &staticDirectory{actions: []*domain.BoundAction{wrapped}}

	// Agent A: has no Skills of its own, only Agent B's Public Skill via
	// its Directory.
	llmA := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
				{ID: "1", Name: "ship_pull_request", Arguments: map[string]any{"change": "add domain model"}},
			}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
			{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "delegated result received"}},
		}}, nil
	}}
	agentA := &domain.Agent{ID: "agent_a", Name: "Agent A", LLMs: []domain.LLM{llmA}, MaxIterations: 5, Directory: directory}

	loopA := domain.NewNativeLoop(agentA.LLMs, agentA.MaxIterations)
	result, transcript, err := loopA.Run(context.Background(), nil, agentA.Directory.PublicSkills(context.Background()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != domain.LoopStatusComplete {
		t.Fatalf("expected LoopStatusComplete, got %s (%s)", result.Status, result.Content)
	}
	if result.Content != "delegated result received" {
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
