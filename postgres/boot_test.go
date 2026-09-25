package postgres_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestLoadAll(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	integration := &domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}
	if err := store.SaveIntegration(ctx, integration); err != nil {
		t.Fatalf("save integration: %v", err)
	}
	tool := &domain.ToolDefinition{ID: "tool_1", Name: "create_pull_request", IntegrationID: integration.ID, Action: "github.create_pull_request"}
	if err := store.SaveTool(ctx, tool); err != nil {
		t.Fatalf("save tool: %v", err)
	}

	agent := &domain.Agent{ID: "agent_1", Name: "Kael Dev", MaxIterations: 10}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	skill := &domain.Skill{
		ID: "skill_1", AgentID: agent.ID, Name: "ship_pr", Visibility: domain.SkillPublic,
		Tools: []domain.ToolBinding{{ToolID: tool.ID}},
	}
	if err := store.SaveSkill(ctx, skill); err != nil {
		t.Fatalf("save skill: %v", err)
	}

	agents, toolsByID, _, integrationsByID, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}

	if len(agents) != 1 || agents[0].ID != agent.ID {
		t.Fatalf("unexpected agents: %+v", agents)
	}
	if len(agents[0].Skills) != 1 || agents[0].Skills[0].ID != skill.ID {
		t.Fatalf("agent's skills not populated: %+v", agents[0].Skills)
	}

	if got, ok := toolsByID[tool.ID]; !ok || got.Action != tool.Action {
		t.Fatalf("unexpected toolsByID: %+v", toolsByID)
	}

	got, ok := integrationsByID[integration.ID]
	if !ok || got.Name != "GitHub" {
		t.Fatalf("unexpected integrationsByID: %+v", integrationsByID)
	}
	if len(got.Tools) != 1 || got.Tools[0].ID != tool.ID {
		t.Fatalf("integration.Tools not populated in integrationsByID: %+v", got.Tools)
	}
}
