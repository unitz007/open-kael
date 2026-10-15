package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
)

// MCPExecutor implements domain.Executor for integrations whose tools are
// served by a remote MCP server over the streamable HTTP transport. The MCP
// wire protocol is a transport detail — callers register and use MCPExecutor
// through domain.ExecutorRegistry the same way as any other Executor:
//
//	registry.Register("github", &runtime.MCPExecutor{Endpoint: "https://mcp.example.com"})
//
// The same Executor instance is shared across every Identity of its service,
// differentiated at call time by identity and connectionRef — identical to the
// contract every other Executor follows (see domain/executor.go).
type MCPExecutor struct {
	// Endpoint is the streamable-HTTP MCP endpoint URL for this service,
	// e.g. "https://mcp.example.com/api". Each Execute or Seed call opens
	// one fresh session against this URL.
	Endpoint string
	// HTTPClient is the underlying HTTP client used for MCP transport.
	// nil uses http.DefaultClient.
	HTTPClient *http.Client
}

// Execute implements domain.Executor. It opens a fresh MCP session, calls the
// named tool (action = MCP tool name), and returns the result. connectionRef,
// when non-empty, is forwarded as Authorization: Bearer — the decrypted OAuth
// token the auth gate already resolved before calling here.
func (e *MCPExecutor) Execute(
	ctx context.Context,
	_ *domain.Identity,
	connectionRef string,
	action string,
	input map[string]any,
) (any, error) {
	session, cleanup, err := e.connect(ctx, connectionRef)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      action,
		Arguments: input,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp executor: call %q: %w", action, err)
	}
	if result.IsError {
		msg := errorText(result.Content)
		return nil, fmt.Errorf("mcp executor: %s: %s", action, msg)
	}
	if result.StructuredContent != nil {
		return result.StructuredContent, nil
	}
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text, nil
		}
	}
	return nil, nil
}

// Seed discovers tools from the MCP server and persists each one via saveFn.
// Call at startup alongside seedPlatform:
//
//	executor.Seed(ctx, integrationID, store.SaveTool)
//
// Existing rows are upserted by saveFn; running Seed on every boot is safe.
func (e *MCPExecutor) Seed(
	ctx context.Context,
	integrationID string,
	saveFn func(context.Context, *domain.ToolDefinition) error,
) error {
	session, cleanup, err := e.connect(ctx, "")
	if err != nil {
		return err
	}
	defer cleanup()

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcp seed %q: list tools: %w", integrationID, err)
	}

	for _, t := range result.Tools {
		def := toolDefFromMCPTool(integrationID, t)
		if err := saveFn(ctx, def); err != nil {
			return fmt.Errorf("mcp seed %q: save tool %q: %w", integrationID, t.Name, err)
		}
	}
	return nil
}

// connect opens a fresh MCP session for one operation, returning the session
// and a cleanup function. connectionRef, when non-empty, is injected as a
// Bearer token on every HTTP request the session makes.
func (e *MCPExecutor) connect(ctx context.Context, connectionRef string) (*mcp.ClientSession, func(), error) {
	httpClient := e.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if connectionRef != "" {
		httpClient = withBearer(httpClient, connectionRef)
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   e.Endpoint,
		HTTPClient: httpClient,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "kael-mcp-executor", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp executor: connect to %q: %w", e.Endpoint, err)
	}
	return session, func() { session.Close() }, nil
}

// withBearer returns a shallow copy of base whose Transport injects
// Authorization: Bearer <token> on every outbound request.
func withBearer(base *http.Client, token string) *http.Client {
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	copy := *base
	copy.Transport = &mcpBearerTransport{token: token, base: rt}
	return &copy
}

type mcpBearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *mcpBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// toolDefFromMCPTool builds a ToolDefinition from an MCP tool listing entry.
// The ToolDefinition's Action is the MCP tool name — the same value Execute
// receives as action — so the executor can dispatch it without any extra lookup.
func toolDefFromMCPTool(integrationID string, t *mcp.Tool) *domain.ToolDefinition {
	return &domain.ToolDefinition{
		ID:            integrationID + "." + t.Name,
		Name:          t.Name,
		FunctionName:  t.Name,
		Description:   t.Description,
		IntegrationID: integrationID,
		Action:        t.Name,
		InputSchema:   anyToSchema(t.InputSchema),
	}
}

// anyToSchema marshals a raw JSON-Schema-shaped value (as the MCP SDK returns
// in tool listings) into a domain.Schema. Returns an empty Schema on error.
func anyToSchema(v any) domain.Schema {
	if v == nil {
		return domain.Schema{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return domain.Schema{}
	}
	var s domain.Schema
	_ = json.Unmarshal(b, &s)
	return s
}

// errorText extracts the first text message from MCP content, returning a
// fallback string when content is empty or contains no text block.
func errorText(content []mcp.Content) string {
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "tool error"
}
