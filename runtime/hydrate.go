package runtime

import "github.com/unitz007/open-kael/domain"

// hydrate resolves a skill's tools against the catalog as it is right now, limited to
// what the agent's creator may use.
func (h *Host) hydrate(hosted *HostedAgent, skill *domain.Skill) ([]*domain.BoundAction, error) {
	tools, identities, integrations := hosted.View()
	return domain.HydrateSkillTools(skill, hosted.Agent, tools, identities, integrations, hosted.Deps.Executors)
}
