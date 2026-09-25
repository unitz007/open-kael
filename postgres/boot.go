package postgres

import (
	"context"
	"fmt"

	"github.com/unitz007/open-kael/domain"
)

// LoadAll reads every stored Identity, Integration (with its Tools), and
// Agent (with its Skills) into the plain in-memory shape
// domain.HydrateSkillTools/domain.AgentRegistry/runtime.Host all already
// consume — the raw hydration step a live boot needs, with no opinion
// about how the result gets wired into a running system (see
// runtime.Boot for that).
//
// What this deliberately does NOT produce: Agent.LLMs, Agent.Loop,
// Agent.Directory, and any Memory/ExecutorRegistry are all live objects,
// never persisted (json:"-" throughout domain/agent.go) — LoadAll cannot
// manufacture a real LLM client or credential resolver out of database
// rows, and doesn't try to. The caller supplies those before anything
// here can actually run.
func (s *Store) LoadAll(ctx context.Context) (agents []*domain.Agent, toolsByID map[string]*domain.ToolDefinition, identitiesByID map[string]*domain.Identity, integrationsByID map[string]*domain.Integration, err error) {
	identities, err := s.ListIdentities(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list identities: %w", err)
	}
	identitiesByID = make(map[string]*domain.Identity, len(identities))
	for _, identity := range identities {
		identitiesByID[identity.ID] = identity
	}

	integrations, err := s.ListIntegrations(ctx)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list integrations: %w", err)
	}

	integrationsByID = make(map[string]*domain.Integration, len(integrations))
	toolsByID = make(map[string]*domain.ToolDefinition)
	for _, integration := range integrations {
		identities, err := s.ListIdentitiesByIntegration(ctx, integration.ID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list identities for integration %q: %w", integration.ID, err)
		}
		integration.Identities = identities

		tools, err := s.ListToolsByIntegration(ctx, integration.ID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list tools for integration %q: %w", integration.ID, err)
		}
		integration.Tools = tools
		integrationsByID[integration.ID] = integration
		for _, tool := range tools {
			toolsByID[tool.ID] = tool
		}
	}

	agentStubs, err := s.ListAgents(ctx, "")
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list agents: %w", err)
	}
	agents = make([]*domain.Agent, 0, len(agentStubs))
	for _, stub := range agentStubs {
		skills, err := s.ListSkillsByAgent(ctx, stub.ID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("postgres: load all: list skills for agent %q: %w", stub.ID, err)
		}
		stub.Skills = skills
		agents = append(agents, stub)
	}

	return agents, toolsByID, identitiesByID, integrationsByID, nil
}
