package domain

import (
	"fmt"
	"strings"
)

// ToolDefinition is a low-level executable primitive provided by exactly one
// Integration — a stored declaration, not a live object. Unlike
// tools.ToolSpec (kael-platform/tools/tool.go), it carries no Go Handler
// closure: Action is a provider-specific dispatch key (e.g.
// "slack.post_message", an MCP tool name) that a future Integration-specific
// executor resolves to real behavior at agent-boot time — the same
// resolution stockai_mcp.go's stockAIToolSpec currently does by hand for one
// hardcoded provider.
type ToolDefinition struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	FunctionName string `json:"function_name,omitempty"`
	Description  string `json:"description"`
	Instructions string `json:"instructions,omitempty"`

	IntegrationID string `json:"integration_id"`

	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`

	// Action is the executor-internal dispatch key (e.g. "slack.post_message").
	// Distinct from FunctionName, which is what the LLM sees in tool-use calls.
	Action string `json:"action"`

	// MaxCalls caps how many times this tool may be called per loop run.
	// 0 means unlimited. Propagated to ActionSpec.MaxCalls at hydration time.
	MaxCalls int `json:"max_calls,omitempty"`

	// RequiresApproval means a human must approve a call before Action
	// actually runs against the resolved Executor — enforced by the
	// runtime host at its single BoundAction.Invoke call site, mirroring
	// kael-platform's tool-definition-level approval gate (see
	// docs/guide/approval.md there): a tool's safety requirement travels
	// with the tool everywhere it's reachable from, rather than depending
	// on every call site remembering to gate it.
	RequiresApproval bool `json:"requires_approval,omitempty"`

	// ApprovalPromptTemplate is the text shown to whoever approves a call,
	// with "{{field}}" placeholders substituted from the call's input (see
	// RenderApprovalPrompt). Kept as a plain template string, not a Go
	// closure, since ToolDefinition is stored data (see the package doc
	// above) — unlike kael-platform's tools.ToolSpec.RequireApproval, whose
	// prompt is a closure, because a ToolSpec is only ever built in code,
	// never persisted. Empty falls back to Description.
	ApprovalPromptTemplate string `json:"approval_prompt_template,omitempty"`
	// Prepared names values its executor's prepare step adds to a call (see Preparer), which the approval prompt may
	// show although the model never gives them: a trade's size worked out from the person's own rule, with what it risks.
	Prepared []string `json:"prepared,omitempty"`
	// PreparedInputs are those of the prepared values a creator may set limits on, with their types ("never more than
	// 100,000 for the size" when the size is worked out from the person's own rule). The model never sees or gives them;
	// limits are checked after the prepare step, so they see what will really be sent.
	PreparedInputs map[string]Schema `json:"prepared_inputs,omitempty"`
	// First names abilities (tool ids) that must have gone through in the same run before this one may: a change that
	// needs what only a look-up gives (a transfer needs the player ids from the person's team). Kept for every agent
	// that has both, exactly like a creator's own "this before that" (prerequisite.go); one it doesn't have is ignored.
	First []string `json:"first,omitempty"`
	// With names look-ups (tool ids) it relies on for its inputs without having to run them first (a transfer finds the
	// player coming in through the player list): wherever it is offered, they are offered too, so trimming what an agent
	// sees never leaves it without the way to its own inputs.
	With []string `json:"with,omitempty"`
	// OnceFor names the inputs that say which thing a change that asks first acts on (an order's order_id; a trade's
	// symbol, direction and levels). In one reply it goes through once for each such thing: a second call for the same
	// values is a repeat and doesn't run, one for other values is another change and does (asking first again). Empty:
	// once per reply, whatever the values. Cancelling all three of a person's waiting orders cancelled the first and
	// answered the other two "already completed", which the model reported as all three done (2026-10-27).
	OnceFor []string `json:"once_for,omitempty"`

	// ApprovalTimeoutSeconds bounds how long the runtime host waits for a
	// response before treating an approval request as declined — never
	// "keep waiting" (same stance kael-platform's approval gate takes).
	// Zero means the runtime host's own default.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`

	// ReadOnly marks the tool as having no side effects — it only reads or
	// fetches data. Used by the runtime to decide which tools are safe to
	// include when the user's intent is advisory (IntentRecommend) rather
	// than actional (IntentExecute).
	ReadOnly bool `json:"read_only,omitempty"`

	// Disabled turns the tool off: it stays listed for whoever manages the
	// integration but is never offered to a skill or run.
	Disabled bool `json:"disabled,omitempty"`

	// NeedsLocalTime marks a tool whose result depends on the person's own clock (a calendar booking
	// "at 10", a day's events, free time). When their time zone is not known yet, the first call asks
	// for it once (see TimezoneRequester) and carries on with what they asked afterwards, instead of
	// quietly assuming UTC.
	NeedsLocalTime bool `json:"needs_local_time,omitempty"`
}

// RenderApprovalPrompt fills ApprovalPromptTemplate's "{{field}}"
// placeholders from input, falling back to Description when no template is
// set. Deliberately plain string substitution, not text/template — an
// approval prompt template is untrusted, dashboard-editable data, and
// text/template's action syntax exposes far more than this needs to.
func (t *ToolDefinition) RenderApprovalPrompt(input map[string]any) string {
	prompt := t.ApprovalPromptTemplate
	if prompt == "" {
		prompt = t.Description
	}
	for key, value := range input {
		prompt = strings.ReplaceAll(prompt, "{{"+key+"}}", fmt.Sprint(value))
	}
	// A line whose value was not given is left out: an approval never shows a placeholder.
	if strings.Contains(prompt, "{{") {
		var kept []string
		for _, line := range strings.Split(prompt, "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		prompt = strings.Join(kept, "\n")
	}
	return prompt
}

// detail is an input a limit may be set on: one the model gives, or one the prepare step works out (PreparedInputs).
func (t *ToolDefinition) detail(name string) (Schema, bool) {
	if p, ok := t.InputSchema.Properties[name]; ok {
		return p, true
	}
	p, ok := t.PreparedInputs[name]
	return p, ok
}

// prepares says whether the tool's prepare step adds the named value (see Prepared).
func (t *ToolDefinition) prepares(name string) bool {
	for _, p := range t.Prepared {
		if p == name {
			return true
		}
	}
	return false
}
