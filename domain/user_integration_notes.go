package domain

// UserIntegrationNotes holds integration-specific facts about a user, learned
// by the agent over time. Keyed by (user_id, integration_id) — shared across
// all agents that have access to the same integration.
type UserIntegrationNotes struct {
	UserID        string
	IntegrationID string
	Notes         string
}
