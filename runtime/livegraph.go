package runtime

import "github.com/unitz007/open-kael/domain"

// LiveGraph supplies integrations, tools and identities that were created
// after boot — a creator's own connection, for instance. The boot maps in
// AgentDeps are shared by every turn and not safe to write while the host
// runs, so later additions live in a graph of their own that guards itself,
// and each hydration reads a consistent snapshot of both.
type LiveGraph interface {
	// Snapshot returns maps the caller may read freely and must not modify.
	Snapshot() (tools map[string]*domain.ToolDefinition, identities map[string]*domain.Identity, integrations map[string]*domain.Integration)
}

// SetLiveGraph attaches the graph of post-boot additions. Call before the host
// starts serving; entries in the graph win over boot entries with the same ID.
func (h *Host) SetLiveGraph(g LiveGraph) { h.live = g }

// graphMaps returns the lookup maps for one hydration: the boot maps, or a
// merged copy of them with the live additions on top.
func (h *Host) graphMaps(d AgentDeps) (map[string]*domain.ToolDefinition, map[string]*domain.Identity, map[string]*domain.Integration) {
	if h.live == nil {
		return d.ToolsByID, d.IdentitiesByID, d.IntegrationsByID
	}
	lt, li, lg := h.live.Snapshot()
	tools := make(map[string]*domain.ToolDefinition, len(d.ToolsByID)+len(lt))
	for k, v := range d.ToolsByID {
		tools[k] = v
	}
	for k, v := range lt {
		tools[k] = v
	}
	identities := make(map[string]*domain.Identity, len(d.IdentitiesByID)+len(li))
	for k, v := range d.IdentitiesByID {
		identities[k] = v
	}
	for k, v := range li {
		identities[k] = v
	}
	integrations := make(map[string]*domain.Integration, len(d.IntegrationsByID)+len(lg))
	for k, v := range d.IntegrationsByID {
		integrations[k] = v
	}
	for k, v := range lg {
		integrations[k] = v
	}
	return tools, identities, integrations
}

// hydrate resolves a skill's tools against the boot graph plus live additions.
func (h *Host) hydrate(hosted *HostedAgent, skill *domain.Skill) ([]*domain.BoundAction, error) {
	tools, identities, integrations := h.graphMaps(hosted.Deps)
	return domain.HydrateSkillTools(skill, hosted.Agent, tools, identities, integrations, hosted.Deps.Executors)
}
