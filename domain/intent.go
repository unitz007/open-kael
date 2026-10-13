package domain

import "context"

// Intent classifies what the user wants to do with the agent.
type Intent string

const (
	// IntentExecute means the user wants to perform an action right now.
	IntentExecute Intent = "execute"
	// IntentRecommend means the user wants advice or information, not an action.
	IntentRecommend Intent = "recommend"
	// IntentSchedule means the user wants to set up, modify, or cancel a recurring task.
	IntentSchedule Intent = "schedule"
)

type intentKey struct{}

// WithIntent stores the classified intent in the context for downstream use.
func WithIntent(ctx context.Context, intent Intent) context.Context {
	return context.WithValue(ctx, intentKey{}, intent)
}

// IntentFromContext retrieves the intent stored by WithIntent.
// Returns IntentExecute and false if none was set.
func IntentFromContext(ctx context.Context) (Intent, bool) {
	v, ok := ctx.Value(intentKey{}).(Intent)
	return v, ok
}
