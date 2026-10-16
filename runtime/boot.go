package runtime

import (
	"log"

	"github.com/unitz007/open-kael/domain"
)

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
	// Catalog, when set, is the catalog everything is registered in, so code that
	// adds connections while the server runs can use the same one. Boot makes one
	// when it is nil.
	Catalog *domain.Catalog
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
	cat := live.Catalog
	if cat == nil {
		cat = domain.NewCatalog()
	}
	registry := domain.NewInMemoryRegistryOn(cat, live.Executors)
	seedCatalog(cat, input)

	host := NewHost()
	for _, agent := range input.Agents {
		agent.LLMs = live.LLMs[agent.ID]
		registry.RegisterAgent(agent)

		host.Register(&HostedAgent{
			Agent: agent,
			Deps: AgentDeps{
				Catalog:   cat,
				Executors: live.Executors,
				Memory:    live.Memory[agent.ID],
			},
		})
	}

	return registry, host
}

// seedCatalog registers everything that was stored, one integration at a time, so
// each integration's tools are held to the tool contract on the way in. A tool that
// breaks it is left out and logged; the rest still load.
func seedCatalog(cat *domain.Catalog, input BootInput) {
	for id, integration := range input.IntegrationsByID {
		src := domain.StaticSource{Integ: integration}
		for _, i := range input.IdentitiesByID {
			if i.IntegrationID == id {
				src.Idents = append(src.Idents, i)
			}
		}
		for _, t := range input.ToolsByID {
			if t.IntegrationID == id {
				src.ToolDefs = append(src.ToolDefs, t)
			}
		}
		if err := cat.AddSource(src); err != nil {
			log.Printf("runtime: boot: integration %q has tools that break the tool contract and were left out:\n%v", id, err)
		}
	}
	// Identities whose integration isn't known still belong in the catalog.
	for _, i := range input.IdentitiesByID {
		if _, ok := input.IntegrationsByID[i.IntegrationID]; !ok {
			cat.PutIdentity(i)
		}
	}
}
