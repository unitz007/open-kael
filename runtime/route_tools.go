package runtime

import (
	"context"
	"log"

	"github.com/unitz007/open-kael/domain"
)

// SetRouteByTools lets the router (when it is a ToolAwareSkillRouter) pick an ability because one of
// its tools fits a request, so a tool added to a connection reaches the agent without anyone
// rewording the ability. Off by default.
func (h *Host) SetRouteByTools(on bool) { h.routeByTools = on }

// routedSkills describes an agent's abilities to a router: each with the tools it can reach. An
// ability whose tools cannot be loaded is still offered, by its description alone.
func (h *Host) routedSkills(ctx context.Context, hosted *HostedAgent) []domain.RoutedSkill {
	out := make([]domain.RoutedSkill, 0, len(hosted.Agent.Skills))
	for _, skill := range hosted.Agent.Skills {
		rs := domain.RoutedSkill{Skill: skill}
		tools, err := h.hydrate(hosted, skill)
		if err != nil {
			log.Printf("skill-router: tools of %q could not be loaded for routing: %v — using its description alone", skill.Name, err)
		}
		for _, t := range tools {
			rs.Tools = append(rs.Tools, domain.RoutedTool{Name: t.Spec.Name, Description: t.Spec.Description})
		}
		out = append(out, rs)
	}
	// The platform's capabilities are read by their own actions.
	for _, p := range platformSkillsFrom(ctx) {
		rs := domain.RoutedSkill{Skill: p.Skill}
		for _, a := range p.Actions {
			rs.Tools = append(rs.Tools, domain.RoutedTool{Name: a.Spec.Name, Description: a.Spec.Description})
		}
		out = append(out, rs)
	}
	return out
}
