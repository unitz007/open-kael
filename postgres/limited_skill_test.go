package postgres_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// A capability limited to some of a connection's abilities keeps that limit across save and load, however it is read back.
func TestSkill_ALimitToSomeAbilitiesSurvivesSavingAndLoading(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Empty(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := postgres.New(pool)
	for _, i := range []*domain.Integration{{ID: "gmail", Name: "Gmail", Service: "google.gmail"}, {ID: "cal", Name: "Calendar", Service: "google.calendar"}} {
		if err := st.SaveIntegration(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	tool := &domain.ToolDefinition{ID: "g_list", Name: "list", IntegrationID: "gmail", Action: "gmail.list", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	if err := st.SaveTool(ctx, tool); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAgent(ctx, &domain.Agent{ID: "a1", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	skill := &domain.Skill{ID: "s1", AgentID: "a1", Name: "mail", Integrations: []string{"gmail", "cal"}, LimitedIntegrations: []string{"gmail"}, Tools: []domain.ToolBinding{{ToolID: "g_list"}}}
	if err := st.SaveSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSkill(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.LimitedIntegrations, []string{"gmail"}) || !reflect.DeepEqual(got.Integrations, []string{"gmail", "cal"}) || len(got.Tools) != 1 || got.Tools[0].ToolID != "g_list" {
		t.Errorf("read back by id: %+v", got)
	}
	list, err := st.ListSkillsByAgent(ctx, "a1")
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].LimitedIntegrations, []string{"gmail"}) {
		t.Errorf("read back with the agent's skills: %+v %v", list, err)
	}
	agent, err := st.LoadAgent(ctx, "a1")
	if err != nil || len(agent.Skills) != 1 || !reflect.DeepEqual(agent.Skills[0].LimitedIntegrations, []string{"gmail"}) {
		t.Errorf("and when the agent is loaded whole: %+v %v", agent, err)
	}

	// taking the limit off is saved too: it widens only when the creator says so
	skill.LimitedIntegrations = nil
	if err := st.SaveSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSkill(ctx, "s1"); len(got.LimitedIntegrations) != 0 {
		t.Errorf("the limit is removed when the capability is saved without it: %+v", got.LimitedIntegrations)
	}
}

// The name a creator gives a capability is kept, read back however it is read, and is separate from its internal name.
func TestSkill_TitleIsKeptApartFromTheInternalName(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Empty(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := postgres.New(pool)
	if err := st.SaveAgent(ctx, &domain.Agent{ID: "a1", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSkill(ctx, &domain.Skill{ID: "s1", AgentID: "a1", Name: "calendar_helper", Title: "Calendar helper", Description: "Checks my calendar"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSkill(ctx, &domain.Skill{ID: "s2", AgentID: "a1", Name: "old_one", Description: "Made before titles"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSkill(ctx, "s1")
	if err != nil || got.Title != "Calendar helper" || got.Name != "calendar_helper" {
		t.Fatalf("read back by id: %+v %v", got, err)
	}
	list, _ := st.ListSkillsByAgent(ctx, "a1")
	byAgents, _ := st.ListSkillsByAgents(ctx, []string{"a1"})
	if len(list) != 2 || list[0].Title != "Calendar helper" || list[1].Title != "" || len(byAgents["a1"]) != 2 || byAgents["a1"][0].Title != "Calendar helper" {
		t.Errorf("with the agent's capabilities, an old one with none: %+v / %+v", list, byAgents)
	}
}

// What a capability says must happen first is kept across save and load, however it is read back, and goes when taken off.
func TestSkill_PrerequisitesSurviveSavingAndLoading(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Empty(t)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := postgres.New(pool)
	if err := st.SaveAgent(ctx, &domain.Agent{ID: "a1", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	want := []domain.Prerequisite{{ID: "p1", Before: "t:place", First: "t:status", Same: []string{"symbol"}, Text: "Before placing a trade, it checks open trades on the same pair"}}
	skill := &domain.Skill{ID: "s1", AgentID: "a1", Name: "trades", Prerequisites: want}
	if err := st.SaveSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSkill(ctx, "s1")
	if err != nil || !reflect.DeepEqual(got.Prerequisites, want) {
		t.Fatalf("read back by id: %+v %v", got, err)
	}
	if agent, err := st.LoadAgent(ctx, "a1"); err != nil || len(agent.Skills) != 1 || !reflect.DeepEqual(agent.Skills[0].Prerequisites, want) {
		t.Errorf("and with the agent: %+v %v", agent, err)
	}
	byAgents, _ := st.ListSkillsByAgents(ctx, []string{"a1"})
	if len(byAgents["a1"]) != 1 || !reflect.DeepEqual(byAgents["a1"][0].Prerequisites, want) {
		t.Errorf("and with several agents': %+v", byAgents)
	}
	skill.Prerequisites = nil
	if err := st.SaveSkill(ctx, skill); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetSkill(ctx, "s1"); got.Prerequisites != nil {
		t.Errorf("taken off when saved without them: %+v", got.Prerequisites)
	}
}
