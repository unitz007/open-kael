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
	// Internal marks a message the loop adds for the model's sake within one run: a nudge ("You must respond by calling
	// one of the available actions…") and the undelivered text that made it necessary. The model sees it during the run;
	// it is never kept in the conversation (see Kept), where it would read later as something the person wrote.
	Internal bool
}

// Kept is the part of a run's messages that belongs in the conversation: everything but what is Internal.
func Kept(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if !m.Internal {
			out = append(out, m)
		}
	}
	return out
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}
