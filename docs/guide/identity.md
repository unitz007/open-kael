# Identity Interface

```go
type Identity interface {
	Provider() string                          // "github", "aws", ...
	ActingAs() string                           // the account/username/role this identity presents as
	Token(ctx context.Context) (string, error)  // a currently-valid credential, refreshed internally if needed
}
```

Use this when a tool needs to act as a specific credentialed identity on an external system — not for the tool itself to define new actions (there's no `Actions()` on this interface), just to declare who the agent is and hand back a live credential. Unlike `Messenger`, there's no common action shape across arbitrary providers — opening a GitHub PR and deploying to AWS share nothing — so `Identity` doesn't define any actions itself. `Token` is a method, not a field, so an implementation can transparently mint/refresh short-lived credentials instead of a caller ever reading a value that's gone stale.

```go
type MyServiceIdentity struct {
	apiKey string
}

func (i *MyServiceIdentity) Provider() string { return "myservice" }
func (i *MyServiceIdentity) ActingAs() string { return "kael-bot" }
func (i *MyServiceIdentity) Token(ctx context.Context) (string, error) {
	return i.apiKey, nil // or mint/refresh a short-lived one here
}
```

Register it on the agent:

```go
a.IdentifyAs(&MyServiceIdentity{apiKey: os.Getenv("MYSERVICE_API_KEY")})
```

Keyed by `Provider()`, so an agent holds at most one declared identity per external system.

Resolve it inside a tool's handler:

```go
func MyServiceTool() *tools.ToolSpec {
	return tools.NewToolBuilder("do_something_on_myservice", "...").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			id, ok := identity.FromContext(ctx, "myservice")
			if !ok {
				return nil, fmt.Errorf("no myservice identity configured for this agent")
			}
			token, err := id.Token(ctx)
			if err != nil {
				return nil, err
			}
			// use token to call the external API
			return result, nil
		}).
		Build()
}
```

This works even for a tool attached to a `Workflow`, built before any specific agent owns it — `identity.FromContext` resolves whichever agent's loop is actually running the tool at call time, not whichever agent the tool happened to be written for. A delegated call correctly resolves against the *target* agent's identities, not the delegator's, because identities are threaded onto `ctx` once, at the top of `runLoopFrom`.
