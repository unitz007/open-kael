package domain

import (
	"context"
	"encoding/json"
	"fmt"
)

// ActionSpec is what an LLM sees for one thing it can invoke. The same
// minimal shape covers a Skill (from the owning Agent's own loop), a
// delegate's Public Skill (SkillContract, addressed across agents), and a
// Tool (from inside a Skill's own nested execution) — deliberately unified,
// since the old framework already proved this works: message_agent is
// structurally just another tools.ToolSpec in the same toolset as
// everything else.
type ActionSpec struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions,omitempty"`
	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`
	// MaxCalls caps how many times this action may be called per loop run.
	// 0 means unlimited. Enforced as a soft block — the loop returns an error
	// result to the model rather than terminating, so it can still call finish.
	MaxCalls int `json:"max_calls,omitempty"`
	// Hidden marks the action as an internal implementation detail. The model
	// can still call it, but the runtime tells it never to describe or mention
	// the action to users.
	Hidden bool `json:"hidden,omitempty"`
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
// One bound Tool: invoked directly, no LLM call needed. More than one: runs
// a nested Loop scoped to just this Skill's own Tools, seeded with its
// Instructions, whose "finish" action is typed to the Skill's own
// OutputSchema — so the nested loop's terminal call IS the structured
// result, landing in LoopResult.Output. Uses agent.Loop if set (see
// Agent's own doc comment — nil falls back to NewNativeLoop), so a Loop
// backed by something other than a per-turn LLM (e.g. a single agentic CLI
// process exposed the Skill's Tools as its own callable actions) can drive
// this the same way.
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
			loop := agent.Loop
			if loop == nil {
				loop = NewNativeLoop(agent.LLMs, agent.MaxIterations)
			}

			// Build the finish schema: start from the skill's OutputSchema and
			// inject a required "content" field so the model always writes a
			// plain-text reply. Without it, result.Content is empty and the
			// caller receives raw JSON (e.g. "{}") instead of a readable message.
			finishSchema := Schema{
				Type:       SchemaTypeObject,
				Properties: make(map[string]Schema, len(skill.OutputSchema.Properties)+1),
				Required:   append(append([]string{}, skill.OutputSchema.Required...), "content"),
			}
			for k, v := range skill.OutputSchema.Properties {
				finishSchema.Properties[k] = v
			}
			finishSchema.Properties["content"] = Schema{
				Type:        SchemaTypeString,
				Description: "A clear, concise summary of what was accomplished, to deliver to the user.",
			}
			finish := &BoundAction{
				Spec: ActionSpec{
					Name:        FinishActionName,
					Description: "Call this once you have the final result.",
					InputSchema: finishSchema,
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
			// Prefer the human-readable content string so the outer loop's LLM
			// receives plain text in its tool-result message, not a JSON-wrapped
			// map that it tends to ignore.
			if result.Content != "" {
				return result.Content, nil
			}
			return result.Output, nil
		},
	}
}
