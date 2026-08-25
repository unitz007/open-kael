package workflow

import (
	"context"

	"github.com/unitz007/kael/tools"
	"github.com/unitz007/kael/triggers"
)

type Workflow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Iteration caps how many loop iterations this workflow's own nested
	// run gets before it's cut off as LLMStatusMaxIteration — a workflow
	// with more steps to work through (e.g. reading several files before
	// writing anything) legitimately needs more room than a simple one.
	// Zero or unset falls back to the agent's own default iteration cap.
	Iteration    int                        `json:"iteration"`
	Description  string                     `json:"description"`
	SystemPrompt string                     `json:"system_prompt"`
	Trigger      triggers.Trigger           `json:"trigger"`
	Tools        map[string]*tools.ToolSpec `json:"tools"`
	// MaxDuplicateToolCallsPerResponse and MaxToolCallsPerResponse override
	// the agent's own defaults (Agent.MaxDuplicateToolCallsPerResponse /
	// Agent.MaxToolCallsPerResponse) for this workflow's own nested run —
	// same reasoning as Iteration overriding MaxIterations: a workflow
	// whose whole job is fanning out over a list (e.g. one tool call per
	// item) has a different natural batch size than the agent's general
	// conversational default, in either direction. Zero or unset falls
	// back to the agent's own value. Ignored (not read at all) when the
	// matching *Func field below is set.
	MaxDuplicateToolCallsPerResponse int `json:"max_duplicate_tool_calls_per_response"`
	MaxToolCallsPerResponse          int `json:"max_tool_calls_per_response"`
	// MaxDuplicateToolCallsPerResponseFunc and MaxToolCallsPerResponseFunc,
	// if set, are called once at the start of each run of this workflow to
	// compute the matching cap dynamically instead of using a fixed number
	// — e.g. asking a real API how many items you're about to fan out over
	// (installed repos, open tickets, whatever your workflow iterates)
	// rather than hardcoding a guess that goes stale the moment that count
	// changes. Takes priority over the plain int field above when set. ctx
	// carries this agent's identities/retrievers the same way a tool
	// handler's ctx does, so the func can call identity.FromContext /
	// rag.FromContext exactly like any other tool would. If it returns an
	// error, that's logged and this workflow's run falls back to the plain
	// int field (then the agent's own default) rather than failing the
	// whole run over what's ultimately a safety-margin lookup, not the
	// workflow's actual job.
	MaxDuplicateToolCallsPerResponseFunc func(ctx context.Context) (int, error) `json:"-"`
	MaxToolCallsPerResponseFunc          func(ctx context.Context) (int, error) `json:"-"`
	// AllowDelegation opts this specific workflow into having the
	// message_agent tool in its nested toolset — off by default.
	// A workflow is meant to be a bounded, single-purpose loop; turn this
	// on only for one whose entire job is deciding what to hand off to
	// which agent (e.g. an issue-triage workflow). Agent.runWorkflow
	// enforces a hard depth cap regardless, so a misconfigured delegation
	// cycle across multiple agents' workflows fails loudly rather than
	// recursing forever.
	AllowDelegation bool `json:"allow_delegation"`
}
