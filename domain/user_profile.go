package domain

// UserProfile holds general facts about a user that the agent learns over time
// across all agents. Keyed by user_id — shared across every agent the user talks to.
type UserProfile struct {
	UserID string
	Notes  string
}
