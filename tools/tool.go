package tools

import (
	"context"
	"encoding/json"
	"time"
)

type HandlerFunc func(ctx context.Context, message json.RawMessage) (any, error)

type ToolRequestParameters struct {
	Name        string
	Description string
	Type        string
	Required    bool
}

type ToolRequestSchema struct {
	Parameters []ToolRequestParameters
}

type ToolSpec struct {
	Name        string
	Description string
	Parameters  []ToolRequestParameters
	Handler     HandlerFunc `json:"-"`
	// Platform restricts this tool to a single messaging platform (e.g.
	// "slack") — set when a tool only makes sense on one platform (Slack
	// reactions, Telegram inline keyboards, ...) and would either error or
	// mean nothing elsewhere. Empty means available on every platform, the
	// default for the vast majority of tools, which have no such
	// restriction.
	Platform string

	// RequiresApproval gates this tool behind an interactive human
	// approve/reject prompt before Handler ever runs — set via
	// ToolSpecBuilder.RequireApproval. Declares only *that* approval is
	// needed, never *how* to obtain it — the mechanism is resolved at call
	// time against whatever messenger is active (see
	// messaging.ApprovalMessenger), so a tool definition itself never
	// references a specific messaging platform.
	RequiresApproval bool `json:"-"`
	// ApprovalSummarize builds the human-readable approval-prompt text
	// from the tool's raw call arguments. Only meaningful when
	// RequiresApproval is true.
	ApprovalSummarize func(ctx context.Context, args json.RawMessage) string `json:"-"`
	// ApprovalTimeout bounds how long to wait before an unanswered prompt
	// is treated as declined. Only meaningful when RequiresApproval is
	// true.
	ApprovalTimeout time.Duration `json:"-"`
}

type ToolSpecBuilder struct {
	toolSpec ToolSpec
}

type ToolCall struct {
	Type      string          `json:"type"`
	Index     int             `json:"index"`
	Id        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// NewToolBuilder creates a new tool builder with the given name and description.
// By default, it creates a function tool with an empty parameter set.
func NewToolBuilder(name, description string) *ToolSpecBuilder {
	return &ToolSpecBuilder{
		toolSpec: ToolSpec{
			Name:        name,
			Description: description,
			Parameters:  make([]ToolRequestParameters, 0),
			Handler:     nil,
		},
	}
}

// Parameter FunctionParameter AddFunctionParameter adds a parameter to the tool's function schema.
// If isRequired is true, the parameter name is added to the Required list.
func (tb *ToolSpecBuilder) Parameter(name, paramType, description string, isRequired bool) *ToolSpecBuilder {
	tb.toolSpec.Parameters = append(tb.toolSpec.Parameters, ToolRequestParameters{
		Name:        name,
		Type:        paramType,
		Description: description,
		Required:    isRequired,
	})

	return tb

}

// Handler AddHarness sets the execution function for this tool.
func (tb *ToolSpecBuilder) Handler(handler HandlerFunc) *ToolSpecBuilder {
	tb.toolSpec.Handler = handler
	return tb
}

// Platform restricts this tool to a single messaging platform — see
// ToolSpec.Platform.
func (tb *ToolSpecBuilder) Platform(platform string) *ToolSpecBuilder {
	tb.toolSpec.Platform = platform
	return tb
}

// RequireApproval gates this tool behind an interactive approve/reject
// prompt before its Handler runs — see ToolSpec.RequiresApproval. summarize
// builds the human-readable prompt text from the tool's raw call
// arguments; timeout bounds how long to wait before an unanswered prompt
// is treated as declined.
func (tb *ToolSpecBuilder) RequireApproval(summarize func(ctx context.Context, args json.RawMessage) string, timeout time.Duration) *ToolSpecBuilder {
	tb.toolSpec.RequiresApproval = true
	tb.toolSpec.ApprovalSummarize = summarize
	tb.toolSpec.ApprovalTimeout = timeout
	return tb
}

// Build returns the completed Tool with the schema and harness function.
func (tb *ToolSpecBuilder) Build() *ToolSpec {
	return &tb.toolSpec
}
