package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// newTestStore connects to DATABASE_URL, runs Migrate, and returns a Store
// wired to a real Postgres instance. These are integration tests, not unit
// tests — skip rather than fail when no database is available, since that
// says nothing about whether the code itself is correct.
func newTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping postgres integration tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("postgres not reachable at DATABASE_URL: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Truncate both before AND after: before, so a prior manual/demo session's
	// leftover rows in this same database (e.g. seeded for the frontend)
	// can't make an unrelated test's row-count assertions flaky; after, so
	// this test doesn't leave anything behind for the next one either.
	truncate := func() {
		_, _ = pool.Exec(context.Background(), `TRUNCATE skill_tools, agent_identities, skills, tools, agents, identities, integrations CASCADE`)
	}
	truncate()
	t.Cleanup(truncate)

	return postgres.New(pool)
}

func TestIntegrationCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	integration := &domain.Integration{
		ID: "int_1", Name: "GitHub", Service: "github",
		Description: "Connected GitHub account",
	}
	if err := store.SaveIntegration(ctx, integration); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.LoadIntegration(ctx, integration.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "GitHub" {
		t.Fatalf("unexpected integration: %+v", got)
	}

	integration.Name = "GitHub (renamed)"
	if err := store.SaveIntegration(ctx, integration); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = store.LoadIntegration(ctx, integration.ID)
	if err != nil || got.Name != "GitHub (renamed)" {
		t.Fatalf("update did not persist: %+v, err=%v", got, err)
	}

	list, err := store.ListIntegrations(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v, err=%v", list, err)
	}

	if err := store.DeleteIntegration(ctx, integration.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.LoadIntegration(ctx, integration.ID); err == nil {
		t.Fatal("expected an error getting a deleted integration")
	}
}

func TestToolCRUDAndSchemaRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	integration := &domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}
	if err := store.SaveIntegration(ctx, integration); err != nil {
		t.Fatalf("save integration: %v", err)
	}

	tool := &domain.ToolDefinition{
		ID: "tool_1", Name: "create_pull_request", Description: "Open a PR",
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
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"url": {Type: domain.SchemaTypeString}},
		},
		Action: "github.create_pull_request",
	}
	if err := store.SaveTool(ctx, tool); err != nil {
		t.Fatalf("save tool: %v", err)
	}

	got, err := store.GetTool(ctx, tool.ID)
	if err != nil {
		t.Fatalf("get tool: %v", err)
	}
	if got.Action != tool.Action || got.InputSchema.Type != domain.SchemaTypeObject {
		t.Fatalf("unexpected tool: %+v", got)
	}
	if len(got.InputSchema.Properties) != 2 || len(got.InputSchema.Required) != 1 || got.InputSchema.Required[0] != "title" {
		t.Fatalf("nested schema did not round-trip: %+v", got.InputSchema)
	}

	tools, err := store.ListToolsByIntegration(ctx, integration.ID)
	if err != nil || len(tools) != 1 {
		t.Fatalf("list tools: %+v, err=%v", tools, err)
	}

	loaded, err := store.LoadIntegration(ctx, integration.ID)
	if err != nil {
		t.Fatalf("load integration: %v", err)
	}
	if len(loaded.Tools) != 1 || loaded.Tools[0].ID != tool.ID {
		t.Fatalf("integration.Tools not populated: %+v", loaded.Tools)
	}
}

func TestAgentAndSkillCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	integration := &domain.Integration{ID: "int_1", Name: "GitHub", Service: "github"}
	tool := &domain.ToolDefinition{ID: "tool_1", Name: "create_pull_request", IntegrationID: integration.ID, Action: "github.create_pull_request"}
	if err := store.SaveIntegration(ctx, integration); err != nil {
		t.Fatalf("save integration: %v", err)
	}
	if err := store.SaveTool(ctx, tool); err != nil {
		t.Fatalf("save tool: %v", err)
	}

	identity := &domain.Identity{ID: "id_1", IntegrationID: integration.ID, Name: "gh-bot", Kind: domain.IdentityKindBot}
	if err := store.SaveIdentity(ctx, identity); err != nil {
		t.Fatalf("save identity: %v", err)
	}

	agent := &domain.Agent{ID: "agent_1", Name: "Kael Dev", MaxIterations: 10, IdentityIDs: []string{identity.ID}}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("save agent: %v", err)
	}

	skill := &domain.Skill{
		ID: "skill_1", AgentID: agent.ID, Name: "ship_pr", Visibility: domain.SkillPublic,
		Tools:   []domain.ToolBinding{{ToolID: tool.ID}},
		Trigger: &domain.Trigger{Type: domain.TriggerTypeCron, Value: "0 9 * * *"},
	}
	if err := store.SaveSkill(ctx, skill); err != nil {
		t.Fatalf("save skill: %v", err)
	}

	got, err := store.GetSkill(ctx, skill.ID)
	if err != nil {
		t.Fatalf("get skill: %v", err)
	}
	if got.AgentID != agent.ID || len(got.Tools) != 1 || got.Tools[0].ToolID != tool.ID {
		t.Fatalf("unexpected skill: %+v", got)
	}
	if got.Trigger == nil || got.Trigger.Type != domain.TriggerTypeCron || got.Trigger.Value != "0 9 * * *" {
		t.Fatalf("trigger did not round-trip: %+v", got.Trigger)
	}

	loaded, err := store.LoadAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if len(loaded.Skills) != 1 || loaded.Skills[0].ID != skill.ID {
		t.Fatalf("agent.Skills not populated: %+v", loaded.Skills)
	}
	if len(loaded.IdentityIDs) != 1 || loaded.IdentityIDs[0] != identity.ID {
		t.Fatalf("agent.IdentityIDs not round-tripped: %+v", loaded.IdentityIDs)
	}

	// Re-saving with a different identity set should replace the join rows.
	agent.IdentityIDs = nil
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("re-save agent: %v", err)
	}
	reloaded, err := store.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("get agent after re-save: %v", err)
	}
	if len(reloaded.IdentityIDs) != 0 {
		t.Fatalf("expected IdentityIDs to be cleared, got %+v", reloaded.IdentityIDs)
	}

	// Re-saving with a different tool set should replace the join rows,
	// not accumulate them.
	skill.Tools = nil
	if err := store.SaveSkill(ctx, skill); err != nil {
		t.Fatalf("re-save skill: %v", err)
	}
	got, err = store.GetSkill(ctx, skill.ID)
	if err != nil || len(got.Tools) != 0 {
		t.Fatalf("expected tool bindings to be cleared: %+v, err=%v", got, err)
	}
}
