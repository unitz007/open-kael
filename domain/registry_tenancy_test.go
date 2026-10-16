package domain_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// Another agent's public skills are offered to every agent, so a skill that runs
// on a creator's own connection (their key, their account) must stay with that
// creator: otherwise anyone else's agent could spend their key.
func TestDelegation_CreatorOwnedConnectionsStayWithTheirCreator(t *testing.T) {
	execs := domain.NewExecutorRegistry()
	execs.Register("platform", echoExecutor{})
	execs.Register("custom_a", echoExecutor{})
	reg := domain.NewInMemoryRegistry(execs)

	reg.RegisterIntegration(&domain.Integration{ID: "platform", Service: "platform"}) // platform-owned
	reg.RegisterIntegration(&domain.Integration{ID: "custom_a", Service: "custom_a", CreatedBy: "creator-a"})
	reg.RegisterTool(&domain.ToolDefinition{ID: "platform.t", Name: "pt", IntegrationID: "platform", Action: "x"})
	reg.RegisterTool(&domain.ToolDefinition{ID: "custom_a.t", Name: "ct", IntegrationID: "custom_a", Action: "x"})

	owner := &domain.Agent{ID: "a1", Name: "A1", CreatedBy: "creator-a", Skills: []*domain.Skill{
		{ID: "s_custom_grant", Name: "uses_custom_grant", Visibility: domain.SkillPublic, Integrations: []string{"custom_a"}},
		{ID: "s_custom_tool", Name: "uses_custom_tool", Visibility: domain.SkillPublic, Tools: []domain.ToolBinding{{ToolID: "custom_a.t"}}},
		{ID: "s_platform", Name: "uses_platform", Visibility: domain.SkillPublic, Integrations: []string{"platform"}},
	}}
	sameCreator := &domain.Agent{ID: "a2", Name: "A2", CreatedBy: "creator-a"}
	otherCreator := &domain.Agent{ID: "b1", Name: "B1", CreatedBy: "creator-b"}
	for _, a := range []*domain.Agent{owner, sameCreator, otherCreator} {
		reg.RegisterAgent(a)
	}

	names := func(a *domain.Agent) map[string]bool {
		out := map[string]bool{}
		for _, b := range a.Directory.PublicSkills(context.Background()) {
			out[b.Spec.Name] = true
		}
		return out
	}

	other := names(otherCreator)
	if other["uses_custom_grant"] || other["uses_custom_tool"] {
		t.Fatalf("another creator's agent was offered a skill that runs on someone else's own connection: %v", other)
	}
	if !other["uses_platform"] {
		t.Fatalf("skills on platform integrations stay available to other agents: %v", other)
	}

	mine := names(sameCreator)
	if !mine["uses_custom_grant"] || !mine["uses_custom_tool"] || !mine["uses_platform"] {
		t.Fatalf("the creator's other agents should still reach their own connection: %v", mine)
	}
}
