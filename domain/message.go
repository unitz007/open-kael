package domain

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message and ToolCall mirror kael-platform/llm.Message and tools.ToolCall
// shape-for-shape (role, content, tool calls, tool-call linkage) — a
// deliberate borrow, since this transcript shape is genuinely
// provider-agnostic and there's no reason to redesign it.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}
