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
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	IntegrationID string `json:"integration_id"`

	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`

	Action string `json:"action"`

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

	// ApprovalTimeoutSeconds bounds how long the runtime host waits for a
	// response before treating an approval request as declined — never
	// "keep waiting" (same stance kael-platform's approval gate takes).
	// Zero means the runtime host's own default.
	ApprovalTimeoutSeconds int `json:"approval_timeout_seconds,omitempty"`
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
	return prompt
}
