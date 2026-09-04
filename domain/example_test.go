package domain_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/unitz007/kael/domain"
)

// echoExecutor is a trivial mock Executor — it just returns whatever input
// it was given, tagged with the action it was dispatched for. Enough to
// prove the Provider -> Executor -> Action link resolves end-to-end at the
// data level, with no real hydration into tools.ToolSpec involved. Shared
// by registry_test.go and hydrate_test.go.
type echoExecutor struct{}

func (echoExecutor) Execute(_ context.Context, integration *domain.Integration, action string, input map[string]any) (any, error) {
	return map[string]any{
		"integration": integration.ID,
		"action":      action,
		"input":       input,
	}, nil
}

// TestAgentPersistenceRoundTrip constructs one Integration owning one
// ToolDefinition, one Skill binding that tool, and one Agent owning that
// skill, then proves the whole Agent survives a JSON round trip unchanged —
// the shapes are persistence-ready.
func TestAgentPersistenceRoundTrip(t *testing.T) {
	integration := &domain.Integration{
		ID:            "int_github",
		Name:          "GitHub",
		Provider:      "github",
		Description:   "Connected GitHub account",
		ConnectionRef: "conn_abc123",
		Status:        domain.IntegrationStatusConnected,
	}

	tool := &domain.ToolDefinition{
		ID:            "tool_create_pr",
		Name:          "create_pull_request",
		Description:   "Open a pull request",
		IntegrationID: integration.ID,
		InputSchema: domain.Schema{
			Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{
				"title": {Type: domain.SchemaTypeString},
				"body":  {Type: domain.SchemaTypeString},
			},
			Required: []string{"title"},
		},
		OutputSchema: domain.Schema{
			Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{
				"url": {Type: domain.SchemaTypeString},
			},
		},
		Action: "github.create_pull_request",
	}
	integration.Tools = []*domain.ToolDefinition{tool}

	skill := &domain.Skill{
		ID:           "skill_ship_pr",
		Name:         "Ship a pull request",
		Description:  "Opens a PR for a completed change",
		Instructions: "Given a title and body, open a pull request.",
		InputSchema:  tool.InputSchema,
		OutputSchema: tool.OutputSchema,
		Tools:        []domain.ToolBinding{{ToolID: tool.ID}},
		Visibility:   domain.SkillPublic,
	}

	agent := &domain.Agent{
		ID:            "agent_dev",
		Name:          "Kael Dev",
		Description:   "Ships code changes",
		Instructions:  "You are a developer agent.",
		Skills:        []*domain.Skill{skill},
		MaxIterations: 10,
	}

	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}

	var roundTripped domain.Agent
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}

	if !reflect.DeepEqual(*agent, roundTripped) {
		t.Fatalf("agent did not survive JSON round trip:\n got:  %+v\n want: %+v", roundTripped, *agent)
	}

	contract := skill.PublicContract()
	if contract.ID != skill.ID || contract.InputSchema.Type != domain.SchemaTypeObject {
		t.Fatalf("unexpected public contract: %+v", contract)
	}
}

// TestExecutorRegistryDispatch proves the Provider -> Executor -> Action
// link: given an Integration and a matching ToolDefinition, the registered
// Executor for that Integration's Provider is found and invoked with the
// right Action and input.
func TestExecutorRegistryDispatch(t *testing.T) {
	integration := &domain.Integration{
		ID:       "int_mock",
		Provider: "mock",
	}
	tool := &domain.ToolDefinition{
		ID:            "tool_echo",
		IntegrationID: integration.ID,
		Action:        "mock.echo",
	}

	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	executor, ok := registry.For(integration)
	if !ok {
		t.Fatalf("expected an executor registered for provider %q", integration.Provider)
	}

	result, err := executor.Execute(context.Background(), integration, tool.Action, map[string]any{"title": "hello"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	if got["action"] != tool.Action || got["integration"] != integration.ID {
		t.Fatalf("unexpected dispatch result: %+v", got)
	}

	if _, ok := registry.For(&domain.Integration{Provider: "unregistered"}); ok {
		t.Fatalf("expected no executor for an unregistered provider")
	}
}
