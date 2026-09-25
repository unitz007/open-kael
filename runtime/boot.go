package runtime

import "github.com/unitz007/open-kael/domain"

// BootInput is the raw, stored-only graph a loader (postgres.Store.LoadAll,
// or any other persistence layer) produces — see postgres/boot.go's own
// doc comment for exactly what it can and cannot contain.
type BootInput struct {
	Agents           []*domain.Agent
	ToolsByID        map[string]*domain.ToolDefinition
	IdentitiesByID   map[string]*domain.Identity
	IntegrationsByID map[string]*domain.Integration
}

// BootLive is everything Boot needs that no persistence layer can ever
// produce, because it's live objects, not data — Agent.LLMs, Agent.Loop,
// Agent.Directory, Memory, and ExecutorRegistry are all json:"-" in
// domain/agent.go for exactly this reason. LLMs and Memory are keyed by
// Agent.ID and both optional per agent: a missing Memory just means that
// agent's turns start cold every time; an agent with no LLMs can't run
// its own NativeLoop, but may still be reachable purely as a delegate
// target for other agents.
type BootLive struct {
	Executors *domain.ExecutorRegistry
	LLMs      map[string][]domain.LLM
	Memory    map[string]domain.Memory
}

// Boot wires a BootInput + BootLive into a live domain.InMemoryRegistry
// (delegation — RegisterAgent sets each Agent's own Directory) and a
// populated *Host (turn handling, listening, cron, webhooks) — the one
// place "everything a loader could hydrate" becomes "everything actually
// needed to run". Integrations and Tools are registered before any Agent
// so a Skill's own dependencies are already present at RegisterAgent
// time — domain.InMemoryRegistry's PublicSkills reads are lazy regardless,
// but there's no reason to rely on that laziness to paper over an
// ordering bug here.
func Boot(input BootInput, live BootLive) (*domain.InMemoryRegistry, *Host) {
	registry := domain.NewInMemoryRegistry(live.Executors)
	for _, identity := range input.IdentitiesByID {
		registry.RegisterIdentity(identity)
	}
	for _, integration := range input.IntegrationsByID {
		registry.RegisterIntegration(integration)
	}
	for _, tool := range input.ToolsByID {
		registry.RegisterTool(tool)
	}

	host := NewHost()
	for _, agent := range input.Agents {
		agent.LLMs = live.LLMs[agent.ID]
		registry.RegisterAgent(agent)

		host.Register(&HostedAgent{
			Agent: agent,
			Deps: AgentDeps{
				ToolsByID:        input.ToolsByID,
				IdentitiesByID:   input.IdentitiesByID,
				IntegrationsByID: input.IntegrationsByID,
				Executors:        live.Executors,
				Memory:           live.Memory[agent.ID],
			},
		})
	}

	return registry, host
}
