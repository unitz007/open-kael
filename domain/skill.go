package domain

import (
	"context"
	"strings"
)

type SkillVisibility string

const (
	SkillPrivate SkillVisibility = "private"
	SkillPublic  SkillVisibility = "public"
)

// SkillOrigin says where a skill came from.
type SkillOrigin string

// SkillLearned is a skill an agent learned in its chats: steps of work it did, kept on a person's yes.
const SkillLearned SkillOrigin = "learned"

// SkillStep is one step of a learned skill: what to do, and the ability (tool id) it uses, or none for a step that is
// only how to go about it ("leave it as a draft").
type SkillStep struct {
	Text    string `json:"text"`
	Ability string `json:"ability,omitempty"`
}

// LearnedSkillStore keeps the skills agents learned in their chats (SaveSkill and DeleteSkill as any skill). They are
// apart from capabilities: nothing that reads an agent's capabilities (routing, what it may use, readiness) sees them.
type LearnedSkillStore interface {
	ListLearnedSkills(ctx context.Context, agentID string) ([]*Skill, error)
}

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
	ID   string `json:"id"`
	Name string `json:"name"`
	// Title is the short name the creator gave it, in their own words ("Calendar helper"), shown wherever the
	// capability is shown. Name stays the internal, model-safe key the router and tool listings use. Empty for
	// capabilities made before titles existed: screens fall back to the Description.
	Title       string `json:"title,omitempty"`
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

	// Integrations is the set of integration IDs this skill is granted access
	// to. When non-empty, the runtime resolves all tools for those integrations
	// dynamically at execution time rather than reading the explicit Tools list.
	// Creators set this via the UI; Tools is then populated at runtime only.
	Integrations []string `json:"integrations,omitempty"`

	// LimitedIntegrations are the connections in Integrations the capability is limited on: for each, only the
	// abilities named in Tools are granted, not the whole connection. Anything not listed here is granted whole,
	// and abilities named in Tools for it are added on top, as they always were. Limiting is explicit so that a
	// connection's later abilities are never granted by surprise and a limit can never quietly widen.
	// Prerequisites are abilities that must run successfully before others, kept by the platform (prerequisite.go).
	Prerequisites       []Prerequisite `json:"prerequisites,omitempty"`
	LimitedIntegrations []string       `json:"limited_integrations,omitempty"`
	// Rules are limits on the details of what this capability does ("never more than 100,000 for the size"), kept by
	// the platform like an agent's own rules (tool_rule.go), but only when the ability is used through this capability.
	Rules []ToolRule `json:"rules,omitempty"`
	// AllLookUps grants, on each connection it is limited on, every ability that only looks things up, including ones
	// the connection gains later, on top of the abilities named in Tools. Changes stay only the ones named. Looking
	// things up is harmless and reads only the account of whoever is talking to it.
	AllLookUps bool `json:"all_look_ups,omitempty"`

	Visibility SkillVisibility `json:"visibility"`

	// Origin is SkillLearned for steps an agent learned in its chats and was told to keep; empty for its own
	// capabilities. A learned skill grants nothing and is never routed to: it is kept apart from Agent.Skills
	// (Agent.Learned), and ListSkillsByAgent never returns one.
	Origin SkillOrigin `json:"origin,omitempty"`
	// PersonID is whose a learned skill is: empty for everyone who talks to the agent, else that person's own.
	PersonID string `json:"person_id,omitempty"`
	// Steps are a learned skill's steps in order; Instructions is them written out.
	Steps []SkillStep `json:"steps,omitempty"`

	// Schedulable declares that users may configure a personal schedule for
	// this skill (time + timezone). The creator sets this flag; the actual
	// schedule is stored per-user, not on the Skill itself.
	Schedulable bool `json:"schedulable,omitempty"`
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

// SkillLabel is how an ability is named to a person: the creator's own description of what
// it does, or its name tidied into words when there is none. It never contains the
// underscores of the internal name.
func SkillLabel(s *Skill) string {
	if s == nil {
		return "your task"
	}
	if d := strings.TrimSpace(s.Description); d != "" {
		return d
	}
	if w := strings.TrimSpace(strings.ReplaceAll(s.Name, "_", " ")); w != "" {
		return w
	}
	return "your task"
}

// MaxSkillTitle is the longest a capability's title may be.
const MaxSkillTitle = 60

// CleanSkillTitle trims a title and folds its whitespace (line breaks included) to single spaces.
func CleanSkillTitle(title string) string {
	return strings.Join(strings.Fields(title), " ")
}

// RulesFor returns the capability's own rules for one ability.
func (s *Skill) RulesFor(toolID string) []ToolRule {
	if s == nil {
		return nil
	}
	var out []ToolRule
	for _, r := range s.Rules {
		if r.ToolID == toolID {
			out = append(out, r)
		}
	}
	return out
}
