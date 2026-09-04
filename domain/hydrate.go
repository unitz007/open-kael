package domain

import (
	"context"
	"fmt"
)

// HydrateTool resolves a stored ToolDefinition (belonging to integration)
// into a real, callable BoundAction — via whatever Executor is registered
// in registry for integration.Provider. This is the seam that turns a
// persisted declaration into something a Loop can actually invoke; see
// Executor's own doc comment for the shape it dispatches through.
func HydrateTool(def *ToolDefinition, integration *Integration, registry *ExecutorRegistry) (*BoundAction, error) {
	if def.IntegrationID != integration.ID {
		return nil, fmt.Errorf("hydrate tool %q: belongs to integration %q, got %q", def.ID, def.IntegrationID, integration.ID)
	}
	executor, ok := registry.For(integration)
	if !ok {
		return nil, fmt.Errorf("hydrate tool %q: no executor registered for provider %q", def.ID, integration.Provider)
	}

	return &BoundAction{
		Spec: ActionSpec{
			Name:         def.Name,
			Description:  def.Description,
			InputSchema:  def.InputSchema,
			OutputSchema: def.OutputSchema,
		},
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			return executor.Execute(ctx, integration, def.Action, input)
		},
	}, nil
}

// HydrateSkillTools resolves every Tool a Skill binds (via ToolBinding)
// into BoundActions, ready to pass to BindSkill. toolsByID and
// integrationsByID are plain lookup maps — this function doesn't care where
// they came from (an in-memory map today, a database query later).
func HydrateSkillTools(skill *Skill, toolsByID map[string]*ToolDefinition, integrationsByID map[string]*Integration, registry *ExecutorRegistry) ([]*BoundAction, error) {
	bound := make([]*BoundAction, 0, len(skill.Tools))
	for _, binding := range skill.Tools {
		def, ok := toolsByID[binding.ToolID]
		if !ok {
			return nil, fmt.Errorf("skill %q: unknown tool %q", skill.ID, binding.ToolID)
		}
		integration, ok := integrationsByID[def.IntegrationID]
		if !ok {
			return nil, fmt.Errorf("skill %q: tool %q references unknown integration %q", skill.ID, def.ID, def.IntegrationID)
		}
		action, err := HydrateTool(def, integration, registry)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", skill.ID, err)
		}
		bound = append(bound, action)
	}
	return bound, nil
}
