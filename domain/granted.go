package domain

import "sort"

// GrantedTools lists the abilities a capability grants, from the abilities that exist: each connection it
// lists whole (or the abilities named for one it is limited on), plus any abilities named on their own.
// It follows exactly what HydrateSkillTools binds, without needing executors or identities, so anything that
// has to know what an agent could do (not what it is doing) reads it from here.
func GrantedTools(skill *Skill, toolsByID map[string]*ToolDefinition) []*ToolDefinition {
	var out []*ToolDefinition
	seen := map[string]bool{}
	add := func(def *ToolDefinition) {
		if def == nil || def.Disabled || seen[def.ID] {
			return
		}
		seen[def.ID] = true
		out = append(out, def)
	}
	for _, b := range skill.Tools {
		add(toolsByID[b.ToolID])
	}
	narrowed := map[string]bool{}
	for _, id := range skill.LimitedIntegrations {
		narrowed[id] = true
	}
	for _, integrationID := range skill.Integrations {
		if narrowed[integrationID] {
			if skill.AllLookUps {
				for _, def := range toolsByID {
					if def.IntegrationID == integrationID && def.ReadOnly {
						add(def)
					}
				}
			}
			continue
		}
		for _, def := range toolsByID {
			if def.IntegrationID == integrationID {
				add(def)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GrantedTools is every ability any of the agent's capabilities grants.
func (a *Agent) GrantedTools(toolsByID map[string]*ToolDefinition) []*ToolDefinition {
	var out []*ToolDefinition
	seen := map[string]bool{}
	for _, s := range a.Skills {
		for _, def := range GrantedTools(s, toolsByID) {
			if !seen[def.ID] {
				seen[def.ID] = true
				out = append(out, def)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
