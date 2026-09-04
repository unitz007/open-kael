package domain_test

import (
	"context"
	"testing"

	"github.com/unitz007/kael/domain"
)

func TestHydrateTool(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	tool := &domain.ToolDefinition{
		ID:            "tool_1",
		Name:          "echo",
		Description:   "Echoes input",
		IntegrationID: integration.ID,
		Action:        "mock.echo",
	}

	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	bound, err := domain.HydrateTool(tool, integration, registry)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if bound.Spec.Name != "echo" {
		t.Fatalf("unexpected spec name: %s", bound.Spec.Name)
	}

	result, err := bound.Invoke(context.Background(), map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	got, ok := result.(map[string]any)
	if !ok || got["action"] != "mock.echo" || got["integration"] != "int_1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHydrateTool_IntegrationMismatch(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	tool := &domain.ToolDefinition{ID: "tool_1", IntegrationID: "int_other"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	if _, err := domain.HydrateTool(tool, integration, registry); err == nil {
		t.Fatal("expected an error for mismatched integration")
	}
}

func TestHydrateTool_NoExecutor(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "unregistered"}
	tool := &domain.ToolDefinition{ID: "tool_1", IntegrationID: integration.ID}
	registry := domain.NewExecutorRegistry()

	if _, err := domain.HydrateTool(tool, integration, registry); err == nil {
		t.Fatal("expected an error for no registered executor")
	}
}

func TestHydrateSkillTools(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Provider: "mock"}
	toolA := &domain.ToolDefinition{ID: "tool_a", Name: "a", IntegrationID: integration.ID, Action: "mock.a"}
	toolB := &domain.ToolDefinition{ID: "tool_b", Name: "b", IntegrationID: integration.ID, Action: "mock.b"}

	skill := &domain.Skill{
		ID:    "skill_1",
		Tools: []domain.ToolBinding{{ToolID: toolA.ID}, {ToolID: toolB.ID}},
	}

	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	bound, err := domain.HydrateSkillTools(
		skill,
		map[string]*domain.ToolDefinition{toolA.ID: toolA, toolB.ID: toolB},
		map[string]*domain.Integration{integration.ID: integration},
		registry,
	)
	if err != nil {
		t.Fatalf("hydrate skill tools: %v", err)
	}
	if len(bound) != 2 || bound[0].Spec.Name != "a" || bound[1].Spec.Name != "b" {
		t.Fatalf("unexpected bound actions: %+v", bound)
	}
}

func TestHydrateSkillTools_UnknownTool(t *testing.T) {
	skill := &domain.Skill{ID: "skill_1", Tools: []domain.ToolBinding{{ToolID: "missing"}}}
	registry := domain.NewExecutorRegistry()

	if _, err := domain.HydrateSkillTools(skill, nil, nil, registry); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}
