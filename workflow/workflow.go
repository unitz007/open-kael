package workflow

import (
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
}
