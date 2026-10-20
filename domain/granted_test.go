package domain_test

import (
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func idsOf(tools []*domain.ToolDefinition) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.ID)
	}
	return out
}

func TestGrantedTools_FollowsWhatHydrationBinds(t *testing.T) {
	tools := map[string]*domain.ToolDefinition{
		"read":   {ID: "read", IntegrationID: "gh", ReadOnly: true},
		"write":  {ID: "write", IntegrationID: "gh"},
		"off":    {ID: "off", IntegrationID: "gh", Disabled: true},
		"other":  {ID: "other", IntegrationID: "cal"},
		"loner":  {ID: "loner", IntegrationID: "cal"},
		"search": {ID: "search", IntegrationID: "web", ReadOnly: true},
	}
	cases := map[string]struct {
		skill *domain.Skill
		want  []string
	}{
		"a whole connection, without what is switched off": {&domain.Skill{Integrations: []string{"gh"}}, []string{"read", "write"}},
		"a limited connection gives only what is named":    {&domain.Skill{Integrations: []string{"gh"}, LimitedIntegrations: []string{"gh"}, Tools: []domain.ToolBinding{{ToolID: "read"}}}, []string{"read"}},
		"named abilities are added on top of a whole one":  {&domain.Skill{Integrations: []string{"gh"}, Tools: []domain.ToolBinding{{ToolID: "other"}}}, []string{"other", "read", "write"}},
		"limiting one connection does not limit another":   {&domain.Skill{Integrations: []string{"gh", "cal"}, LimitedIntegrations: []string{"gh"}, Tools: []domain.ToolBinding{{ToolID: "read"}}}, []string{"loner", "other", "read"}},
		"a limit on a connection not listed grants none":   {&domain.Skill{Integrations: []string{"cal"}, LimitedIntegrations: []string{"gh"}}, []string{"loner", "other"}},
		"an unknown ability is skipped":                    {&domain.Skill{Tools: []domain.ToolBinding{{ToolID: "ghost"}}}, nil},
	}
	for name, tc := range cases {
		got := idsOf(domain.GrantedTools(tc.skill, tools))
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", name, got, tc.want)
			}
		}
	}
}

func TestAgentGrantedTools_IsTheUnionWithoutRepeats(t *testing.T) {
	tools := map[string]*domain.ToolDefinition{"a": {ID: "a", IntegrationID: "x"}, "b": {ID: "b", IntegrationID: "y"}}
	agent := &domain.Agent{Skills: []*domain.Skill{{Integrations: []string{"x"}}, {Integrations: []string{"x", "y"}}}}
	if got := idsOf(agent.GrantedTools(tools)); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("%v", got)
	}
}
