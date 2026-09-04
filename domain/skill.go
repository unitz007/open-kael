package domain

type SkillVisibility string

const (
	SkillPrivate SkillVisibility = "private"
	SkillPublic  SkillVisibility = "public"
)

// ToolBinding references a Tool by ID so a Skill doesn't duplicate the Tool
// definition — Tools are reusable across many Skills.
type ToolBinding struct {
	ToolID string `json:"tool_id"`
}

// Skill is a meaningful capability owned by one Agent, built from one or
// more Tools. This is the public capability surface: other agents discover
// Skills (name, description, input/output schema), never the raw Tools a
// Skill happens to use internally. There is deliberately no separate
// workflow.Workflow-equivalent type: Trigger folds that concept in here — a
// Skill is both a typed, synchronous contract (invocable directly, or by
// another agent's delegate call) AND, when Trigger is set, something that
// also fires on its own. See BindSkill (action.go) for execution mechanics.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// AgentID is the owning Agent's ID — the explicit back-reference for
	// the relationship Agent.Skills otherwise only expresses one direction
	// (owner -> owned). Mirrors ToolDefinition.IntegrationID, which already
	// does this for Tool -> Integration; a persistence layer needs this
	// field to store the relationship at all.
	AgentID string `json:"agent_id"`

	Instructions string `json:"instructions"`

	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`

	// Internal implementation primitives — never exposed directly (see
	// Visibility and the package doc comment).
	Tools []ToolBinding `json:"tools"`

	Visibility SkillVisibility `json:"visibility"`

	// Trigger is optional — nil means purely invoked on demand (by the
	// owning Agent's own loop, or by another agent's delegate call). Set,
	// it also fires autonomously — the replacement for what
	// workflow.Workflow's Trigger field did in the old framework.
	Trigger *Trigger `json:"trigger,omitempty"`
}

// PublicContract returns only what a Public Skill exposes to other agents —
// never Tools, which stay an implementation detail regardless of Visibility.
func (s *Skill) PublicContract() SkillContract {
	return SkillContract{
		ID:           s.ID,
		Name:         s.Name,
		Description:  s.Description,
		InputSchema:  s.InputSchema,
		OutputSchema: s.OutputSchema,
	}
}

// SkillContract is what gets serialized into another agent's discovery view
// — the structured replacement for today's free-text DelegateCapabilities().
type SkillContract struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`
}
