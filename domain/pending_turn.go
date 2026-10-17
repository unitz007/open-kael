package domain

import "time"

// PendingTurn is what a person asked for while something stood in the way (an
// account that had to be connected first), kept so it can be carried out the moment
// the way is clear instead of making them ask again.
type PendingTurn struct {
	UserID     string
	IdentityID string // the connection that was missing
	AgentID    string
	Conv       ConversationRef
	Message    string
	CreatedAt  time.Time
}
