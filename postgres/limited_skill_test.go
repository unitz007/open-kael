package postgres_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// A capability limited to some of a connection's abilities keeps that limit across save and load, however it is read back.
func TestSkill_ALimitToSomeAbilitiesSurvivesSavingAndLoading(t *testing.T) {
	ctx := context.Background()
	pool := scratchDB(t)
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
