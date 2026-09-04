package domain

import "context"

// Executor performs one Provider's tools — implemented once per Provider
// (e.g. "slack", "github", "local"), not once per Tool. It resolves
// integration.ConnectionRef into real credentials internally and dispatches
// on action, which matches ToolDefinition.Action for any ToolDefinition
// belonging to this Integration. input is already decoded against the
// ToolDefinition's InputSchema by the caller.
//
// This is the seam HydrateTool (hydrate.go) calls through to turn a stored
// ToolDefinition + its Integration into a live, callable BoundAction.
type Executor interface {
	Execute(ctx context.Context, integration *Integration, action string, input map[string]any) (any, error)
}

// ExecutorRegistry looks up the Executor responsible for one Integration's
// Provider. One registry entry per provider, shared across every Integration
// instance of that provider.
type ExecutorRegistry struct {
	executors map[string]Executor
}

func NewExecutorRegistry() *ExecutorRegistry {
	return &ExecutorRegistry{executors: make(map[string]Executor)}
}

func (r *ExecutorRegistry) Register(provider string, e Executor) {
	r.executors[provider] = e
}

func (r *ExecutorRegistry) For(integration *Integration) (Executor, bool) {
	e, ok := r.executors[integration.Provider]
	return e, ok
}
