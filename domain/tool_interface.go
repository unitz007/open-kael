package domain

import "context"

// Tool is the one thing an agent can use. A built-in tool, one described by an
// OpenAPI file and one served by a tool server are all a Tool once bound, and the
// runtime knows nothing else about where it came from.
type Tool interface {
	// Spec is everything the model is told and the contract checks.
	Spec() ToolSpec

	// Invoke runs the tool. Every gate the platform requires (input check, approval,
	// sign-in) is already part of it; input reaches the underlying executor
	// checked against Spec().Input.
	Invoke(ctx context.Context, input map[string]any) (any, error)
}

// ToolSpec is the tool contract as data.
type ToolSpec struct {
	ID           string
	SourceID     string // the integration it belongs to
	Name         string // the name the model sees
	Title        string // the plain-words name shown to people
	Description  string
	Instructions string
	Input        Schema
	Output       Schema
	// Effect is Reads or Changes. There is no "unknown".
	Effect   Effect
	Approval Approval
	MaxCalls int
}

// Approval is when and how a person is asked before the tool runs.
type Approval struct {
	Required       bool
	PromptTemplate string
	TimeoutSeconds int
}

// Spec is the contract view of a stored tool.
func (t *ToolDefinition) Spec() ToolSpec {
	return ToolSpec{
		ID: t.ID, SourceID: t.IntegrationID, Name: t.ModelName(), Title: t.Name,
		Description: t.Description, Instructions: t.Instructions,
		Input: t.InputSchema, Output: t.OutputSchema,
		Effect:   t.Effect(),
		Approval: Approval{Required: t.RequiresApproval, PromptTemplate: t.ApprovalPromptTemplate, TimeoutSeconds: t.ApprovalTimeoutSeconds},
		MaxCalls: t.MaxCalls,
	}
}

type boundTool struct {
	spec   ToolSpec
	invoke func(ctx context.Context, input map[string]any) (any, error)
}

var _ Tool = (*boundTool)(nil)

func (b *boundTool) Spec() ToolSpec { return b.spec }
func (b *boundTool) Invoke(ctx context.Context, input map[string]any) (any, error) {
	return b.invoke(ctx, input)
}

// ActionOf adapts a Tool to the BoundAction the agent loop and the model clients
// are written against. It adds nothing: the action runs the tool itself.
func ActionOf(t Tool) *BoundAction {
	s := t.Spec()
	return &BoundAction{
		Spec: ActionSpec{
			Name:         s.Name,
			Description:  s.Description,
			Instructions: s.Instructions,
			InputSchema:  s.Input,
			OutputSchema: s.Output,
			MaxCalls:     s.MaxCalls,
			ReadOnly:     s.Effect == EffectReads,
			Changes:      s.Effect == EffectChanges,
		},
		Invoke: t.Invoke,
	}
}
