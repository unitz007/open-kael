package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// newTestStore is a Store over a migrated database of the test's own (see pgtest). These are
// integration tests, not unit tests — skip rather than fail when no database is available, since
// that says nothing about whether the code itself is correct.
func newTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	store, _ := pgtest.Store(t)
	return store
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
		Tools: []domain.ToolBinding{{ToolID: tool.ID}},
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

// The rules a creator sets live with the agent: saved, loaded back the way they went in, kept when
// the agent is saved again from what was loaded (as every update does), and gone with the agent.
func TestAgentRulesRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	agent := &domain.Agent{ID: "ag_rules", Name: "Desk", Rules: []domain.ToolRule{
		{ID: "r1", ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 100000.0, Message: "Too big."},
		{ID: "r2", ToolID: "tool_trade", Input: "symbol", Kind: domain.RuleOneOf, Value: []any{"EURUSD", "GBPUSD"}},
		{ID: "r3", ToolID: "tool_trade", Input: "min_rr", Kind: domain.RuleFixed, Value: 2.0},
		{ID: "r4", ToolID: "tool_trade", Input: "size", With: "entry", Kind: domain.RuleMaxProduct, Value: 250000.0},
	}}
	if err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.LoadAgent(ctx, "ag_rules")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != 4 || got.Rules[3].With != "entry" || got.Rules[3].Kind != domain.RuleMaxProduct {
		t.Fatalf("a value limit keeps its second detail: %+v", got.Rules)
	}
	if got.Rules[0].Kind != domain.RuleMax || got.Rules[0].Value != 100000.0 || got.Rules[0].Message != "Too big." {
		t.Fatalf("rules come back as saved: %+v", got.Rules)
	}
	if list, _ := got.Rules[1].Value.([]any); len(list) != 2 || list[0] != "EURUSD" {
		t.Errorf("a list of values comes back as a list: %#v", got.Rules[1].Value)
	}

	// Every update loads the agent, changes one thing and saves it back: the rules must survive that.
	got.Description = "Trades currencies"
	if err := store.SaveAgent(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := store.GetAgent(ctx, "ag_rules")
	if len(again.Rules) != 4 {
		t.Errorf("rules survive an unrelated update: %+v", again.Rules)
	}

	// An agent with no rules reads back with none.
	if err := store.SaveAgent(ctx, &domain.Agent{ID: "ag_plain", Name: "Plain"}); err != nil {
		t.Fatal(err)
	}
	if plain, _ := store.GetAgent(ctx, "ag_plain"); len(plain.Rules) != 0 {
		t.Errorf("no rules: %+v", plain.Rules)
	}

	// Clearing them is saving the agent with none.
	again.Rules = nil
	if err := store.SaveAgent(ctx, again); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := store.GetAgent(ctx, "ag_rules"); len(cleared.Rules) != 0 {
		t.Errorf("rules cleared: %+v", cleared.Rules)
	}
}

// A tool that depends on the person's own clock keeps saying so, through every way a tool is read.
func TestToolNeedsLocalTimeRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "int_nlt", Name: "Calendar", Service: "cal"}); err != nil {
		t.Fatal(err)
	}
	for _, def := range []*domain.ToolDefinition{
		{ID: "nlt_a", Name: "book", IntegrationID: "int_nlt", Action: "a", NeedsLocalTime: true},
		{ID: "nlt_b", Name: "list_calendars", IntegrationID: "int_nlt", Action: "b", ReadOnly: true},
	} {
		if err := store.SaveTool(ctx, def); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := store.GetTool(ctx, "nlt_a")
	b, _ := store.GetTool(ctx, "nlt_b")
	if !a.NeedsLocalTime || b.NeedsLocalTime {
		t.Errorf("GetTool: %v %v", a.NeedsLocalTime, b.NeedsLocalTime)
	}
	list, err := store.ListToolsByIntegration(ctx, "int_nlt")
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	for _, d := range list {
		if d.NeedsLocalTime != (d.ID == "nlt_a") {
			t.Errorf("ListToolsByIntegration %s: %v", d.ID, d.NeedsLocalTime)
		}
	}
	a.NeedsLocalTime = false
	if err := store.SaveTool(ctx, a); err != nil {
		t.Fatal(err)
	}
	if again, _ := store.GetTool(ctx, "nlt_a"); again.NeedsLocalTime {
		t.Error("saving again can turn it off")
	}
}

// A tool's prepared values and its per-run limit survive being stored, so a tool whose approval shows a prepared value
// (a trade's worked-out size) still keeps the tool contract when it is loaded. Without them place_trade was left out at
// every start (2026-10-23).
func TestToolPreparedAndMaxCallsRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.SaveIntegration(ctx, &domain.Integration{ID: "int_prep", Name: "Aegis", Service: "trading"}); err != nil {
		t.Fatal(err)
	}
	def := &domain.ToolDefinition{ID: "prep_trade", Name: "place_trade", Description: "Places a trade", IntegrationID: "int_prep", Action: "trade",
		RequiresApproval: true, ApprovalTimeoutSeconds: 300, ApprovalPromptTemplate: "Size: {{size_line}}\n{{rr_line}}",
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString, Description: "The pair."}}},
		Prepared:    []string{"size_line", "rr_line"}, MaxCalls: 1}
	if err := store.SaveTool(ctx, def); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTool(ctx, "prep_trade")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Prepared, ",") != "size_line,rr_line" || got.MaxCalls != 1 {
		t.Fatalf("kept: prepared=%v max_calls=%d", got.Prepared, got.MaxCalls)
	}
	if err := domain.ValidateTool(got); err != nil {
		t.Errorf("a loaded tool keeps the contract: %v", err)
	}
	list, _ := store.ListToolsByIntegration(ctx, "int_prep")
	if len(list) != 1 || len(list[0].Prepared) != 2 || list[0].MaxCalls != 1 {
		t.Errorf("listed the same: %+v", list)
	}
	plain := &domain.ToolDefinition{ID: "prep_plain", Name: "look", Description: "Looks", IntegrationID: "int_prep", Action: "look", ReadOnly: true,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	if err := store.SaveTool(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.GetTool(ctx, "prep_plain"); len(p.Prepared) != 0 || p.MaxCalls != 0 {
		t.Errorf("a tool with neither stays without: %+v", p)
	}
}
