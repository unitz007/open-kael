package domain

import (
	"context"
	"encoding/json"
	"fmt"
)

// ActionSpec is what an LLM sees for one thing it can invoke. The same
// minimal shape covers a Skill (from the owning Agent's own loop), a
// delegate's Public Skill (SkillContract, addressed across agents), and a
// Tool (from inside a Skill's own nested execution) — deliberately unified.
type ActionSpec struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`
}

// BoundAction pairs an ActionSpec with what actually runs it. Invoke is
// resolved at hydration time — from an Executor (for a Tool), a nested
// Loop.Run call (for one of the Agent's own Skills — see BindSkill), or a
// call into another Agent's Directory (for a delegate Skill) — the Loop
// never needs to know which.
type BoundAction struct {
	Spec   ActionSpec
	Invoke func(ctx context.Context, input map[string]any) (any, error)
}

// BindSkill turns a Skill plus its already-resolved Tools (bound via
// ToolBinding -> ToolDefinition -> Executor, outside this function's
// concern) into one BoundAction other loops can call without knowing
// whether a Skill runs as a single direct call or a nested multi-step loop.
//
// One bound Tool: invoked directly, no LLM call needed. More than one:
// runs a nested NativeLoop scoped to just this Skill's own Tools, seeded
// with its Instructions, whose "finish" action is typed to the Skill's own
// OutputSchema — so the nested loop's terminal call IS the structured
// result, landing in LoopResult.Output.
func BindSkill(agent *Agent, skill *Skill, tools []*BoundAction) *BoundAction {
	spec := ActionSpec{
		Name:         skill.Name,
		Description:  skill.Description,
		InputSchema:  skill.InputSchema,
		OutputSchema: skill.OutputSchema,
	}

	if len(tools) == 1 {
		only := tools[0]
		return &BoundAction{
			Spec: spec,
			Invoke: func(ctx context.Context, input map[string]any) (any, error) {
				return only.Invoke(ctx, input)
			},
		}
	}

	return &BoundAction{
		Spec: spec,
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			loop := NewNativeLoop(agent.LLMs, agent.MaxIterations)

			finish := &BoundAction{
				Spec: ActionSpec{
					Name:        FinishActionName,
					Description: "Call this once you have the final result.",
					InputSchema: skill.OutputSchema,
				},
			}
			actions := make([]*BoundAction, 0, len(tools)+1)
			actions = append(actions, tools...)
			actions = append(actions, finish)

			inputJSON, err := json.Marshal(input)
			if err != nil {
				return nil, fmt.Errorf("skill %q: encode input: %w", skill.Name, err)
			}

			messages := []Message{
				{Role: RoleSystem, Content: skill.Instructions},
				{Role: RoleUser, Content: string(inputJSON)},
			}

			result, _, err := loop.Run(ctx, messages, actions)
			if err != nil {
				return nil, fmt.Errorf("skill %q: %w", skill.Name, err)
			}
			if result.Status != LoopStatusComplete {
				return nil, fmt.Errorf("skill %q did not complete: %s", skill.Name, result.Status)
			}
			return result.Output, nil
		},
	}
}
