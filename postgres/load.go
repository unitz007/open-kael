package postgres

import (
	"context"
	"fmt"

	"github.com/unitz007/open-kael/domain"
)

// LoadIntegration returns an Integration with its Tools populated —
// Integration.Tools is a populate-on-read convenience (see
// documentation.md's Relationships section); ToolDefinition.IntegrationID
// stays the source of truth.
func (s *Store) LoadIntegration(ctx context.Context, id string) (*domain.Integration, error) {
	integration, err := s.getIntegrationRow(ctx, id)
	if err != nil {
		return nil, err
	}
	identities, err := s.ListIdentitiesByIntegration(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: load identities for integration %q: %w", id, err)
	}
	integration.Identities = identities
	tools, err := s.ListToolsByIntegration(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: load tools for integration %q: %w", id, err)
	}
	integration.Tools = tools
	return integration, nil
}

// LoadAgent returns an Agent with its Skills populated — same
// populate-on-read pattern as LoadIntegration, for Agent.Skills.
func (s *Store) LoadAgent(ctx context.Context, id string) (*domain.Agent, error) {
	agent, err := s.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	skills, err := s.ListSkillsByAgent(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: load skills for agent %q: %w", id, err)
	}
	agent.Skills = skills
	return agent, nil
}
