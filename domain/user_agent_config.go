package domain

// UserAgentConfig holds per-user customisation for one Agent — currently
// just freeform personal instructions that the user provides during onboarding
// or edits later via /instructions. These are injected into the system prompt
// as a <user_instructions> block so the agent can tailor its responses.
type UserAgentConfig struct {
	UserID       string `json:"user_id"`
	AgentID      string `json:"agent_id"`
	Instructions string `json:"instructions"`
}
