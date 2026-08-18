# Tools

## API Reference

```go
tools.NewToolBuilder(name, description string) *ToolSpecBuilder
    .Parameter(name, paramType, description string, isRequired bool) *ToolSpecBuilder  // repeatable
    .RequireApproval(summarize func(ctx, args) string, timeout time.Duration) *ToolSpecBuilder // see Approval-Gated Tools
    .Handler(func(ctx context.Context, args json.RawMessage) (any, error)) *ToolSpecBuilder
    .Build() *ToolSpec
```

## Reading arguments

Tool arguments arrive **double-encoded**: `args` is a `json.RawMessage` whose contents are a JSON *string*, which itself contains the actual JSON object. Every tool handler in this module unwraps it the same two-step way — copy this pattern for a new tool:

```go
func MyTool() *tools.ToolSpec {
	return tools.NewToolBuilder("my_tool", "Does something useful").
		Parameter("message", "string", "The message to process", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}

			var input struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			// ... do the work ...
			return "done", nil
		}).
		Build()
}
```

## A tool without parameters

```go
func GetSecretCodeTool() *tools.ToolSpec {
	return tools.NewToolBuilder("get_secret_code", "Looks up the current secret code").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			return secretCode, nil
		}).
		Build()
}
```

## Attaching a tool

**Agent-level** (available on every message the agent handles, in a workflow's nested run, and in a delegated call):

```go
a := agent.NewAgent(...)
a.AddTool(MyTool())
```

**Workflow-level** (only available while that workflow's own nested loop is running):

```go
myWorkflow := workflow.Workflow{
	// ...
	Tools: map[string]*tools.ToolSpec{
		"my_tool": MyTool(),
	},
}
```

## Repeat calls

There's no per-tool opt-in for this — `runLoopFrom` itself blocks any tool from being called a second time with the *exact same arguments* once it's already succeeded once, regardless of which tool it is. This is enough to stop a model from re-sending the same message or re-delegating the same task after success, without needing a tool author to declare anything.

## Approval gating

A tool can also require a human's sign-off before its `Handler` runs at all — see [Approval-Gated Tools](approval.md) for the full mechanism.
