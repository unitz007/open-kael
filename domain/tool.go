package domain

// ToolDefinition is a low-level executable primitive provided by exactly one
// Integration — a stored declaration, not a live object. Unlike
// tools.ToolSpec (kael-platform/tools/tool.go), it carries no Go Handler
// closure: Action is a provider-specific dispatch key (e.g.
// "slack.post_message", an MCP tool name, or an existing tool's own Name
// under the "local" provider) that an Integration-specific Executor
// resolves to real behavior at agent-boot time.
type ToolDefinition struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	IntegrationID string `json:"integration_id"`

	InputSchema  Schema `json:"input_schema"`
	OutputSchema Schema `json:"output_schema"`

	Action string `json:"action"`
}
