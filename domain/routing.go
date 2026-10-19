package domain

// RoutedSkill is an ability together with what its tools do, for a router that judges tools as well
// as the ability's own description. Without this, an ability is only as findable as its one-line
// description is complete: a tool added later never makes the ability look relevant.
type RoutedSkill struct {
	Skill *Skill
	Tools []RoutedTool
}

// RoutedTool is what a router is told about one tool: its name and what it does.
type RoutedTool struct {
	Name        string
	Description string
}

// Key identifies a tool across abilities: two abilities that grant the same connection share its
// tools, and a router asks about each such tool once.
func (t RoutedTool) Key() string { return t.Name + "\x00" + t.Description }
