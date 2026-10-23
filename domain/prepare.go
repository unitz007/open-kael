package domain

import (
	"context"
	"fmt"
)

// Preparer is an executor that fills in or adds to a call's inputs before anyone is asked to approve it: for example
// working out a trade's size from the person's own sizing rule and their live balance, so the approval shows (and the
// creator's rules check) exactly what will be sent. It runs after the input check and before the creator's rules, the
// approval and the executor itself. What it returns replaces the inputs; an error goes back to the model, in plain words,
// and nothing is approved or run. connectionRef is the person's own connection, empty when they have not connected yet
// (prepare should then return the inputs unchanged and let the sign-in gate ask them to connect).
type Preparer interface {
	Prepare(ctx context.Context, identity *Identity, connectionRef, action string, input map[string]any) (map[string]any, error)
}

func withPrepare(p Preparer, def *ToolDefinition, identity *Identity, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		var connectionRef string
		if identity != nil {
			connectionRef, _ = ConnectionRefFromContext(ctx, identity.ID)
		}
		prepared, err := p.Prepare(ctx, identity, connectionRef, def.Action, input)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", def.ModelName(), err)
		}
		if prepared == nil {
			prepared = input
		}
		return inner(ctx, prepared)
	}
}
