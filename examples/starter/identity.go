package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/unitz007/kael/identity"
	"github.com/unitz007/kael/tools"
)

// exampleIdentity is a minimal identity.Identity — enough to show the shape
// (Provider/ActingAs/Token), not a real integration. A real one (a GitHub
// App installation token, AWS STS, whatever) would mint/refresh a
// short-lived credential inside Token instead of just reading a static env
// var — see kael-platform's README for why Token is a method, not a field.
type exampleIdentity struct {
	token string
}

func newExampleIdentity() identity.Identity {
	return &exampleIdentity{token: os.Getenv("EXAMPLE_API_TOKEN")}
}

func (e *exampleIdentity) Provider() string { return "example" }
func (e *exampleIdentity) ActingAs() string { return "starter-bot" }

func (e *exampleIdentity) Token(ctx context.Context) (string, error) {
	if e.token == "" {
		return "", fmt.Errorf("EXAMPLE_API_TOKEN not set")
	}
	return e.token, nil
}

// whoAmITool shows how any tool resolves a registered identity: never
// closed over at construction time (a workflow's tools are built before the
// agent that will eventually own them even exists), always looked up via
// identity.FromContext at call time instead.
func whoAmITool() *tools.ToolSpec {
	return tools.NewToolBuilder("whoami", "Reports which identity this agent is currently acting as on the \"example\" provider.").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			id, ok := identity.FromContext(ctx, "example")
			if !ok {
				return "no \"example\" identity registered on this agent", nil
			}
			token, err := id.Token(ctx)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("acting as %q on %q (token present: %v)", id.ActingAs(), id.Provider(), token != ""), nil
		}).Build()
}
