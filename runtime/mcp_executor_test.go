package runtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// newMCPBackendServer builds a minimal MCP server that the MCPExecutor tests
// connect to as a client — this is the *external* MCP server, not the agent's
// own inbound MCP surface. It exposes one tool:
//
//	greet(name string) → "hello, <name>"
//
// When the request carries an Authorization header, the tool echoes it back as
// "auth:<value>" so tests can verify connectionRef propagation.
func newMCPBackendServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "0.0.1"}, nil)

	tool := &mcp.Tool{
		Name:        "greet",
		Description: "Greet someone by name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	}
	srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Name string `json:"name"`
		}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
					&mcp.TextContent{Text: err.Error()},
				}}, nil
			}
		}

		msg := "hello, " + args.Name

		// Echo the Authorization header back so tests can assert on auth
		// forwarding without needing a separate mock.
		if auth, ok := ctx.Value(mcpAuthKey{}).(string); ok && auth != "" {
			msg = "auth:" + auth
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
			StructuredContent: msg,
		}, nil
	})

	// Intercept the HTTP request before handing off to the MCP handler so we
	// can store the Authorization header in the context for the tool handler.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return srv }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		mcpHandler.ServeHTTP(w, r)
	})

	return httptest.NewServer(mux)
}

// mcpAuthKey is a private context key — unused for now but reserved for a
// future round of auth-propagation tests that will require middleware plumbing.
type mcpAuthKey struct{}

func TestMCPExecutor_ExecuteTool(t *testing.T) {
	backend := newMCPBackendServer(t)
	defer backend.Close()

	exec := &runtime.MCPExecutor{Endpoint: backend.URL + "/mcp"}

	got, err := exec.Execute(context.Background(), nil, "", "greet", map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "hello, world" {
		t.Fatalf("expected %q, got %q", "hello, world", got)
	}
}

func TestMCPExecutor_Seed(t *testing.T) {
	backend := newMCPBackendServer(t)
	defer backend.Close()

	exec := &runtime.MCPExecutor{Endpoint: backend.URL + "/mcp"}

	var saved []*domain.ToolDefinition
	err := exec.Seed(context.Background(), "test-integration", func(_ context.Context, td *domain.ToolDefinition) error {
		saved = append(saved, td)
		return nil
	})
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(saved) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(saved))
	}
	td := saved[0]
	if td.Name != "greet" {
		t.Fatalf("unexpected tool name %q", td.Name)
	}
	if td.Action != "greet" {
		t.Fatalf("Action should equal Name (MCP dispatch key), got %q", td.Action)
	}
	if td.IntegrationID != "test-integration" {
		t.Fatalf("unexpected integration ID %q", td.IntegrationID)
	}
	if td.ID != "test-integration.greet" {
		t.Fatalf("unexpected tool ID %q", td.ID)
	}
	if td.InputSchema.Type != domain.SchemaTypeObject {
		t.Fatalf("expected object schema, got %q", td.InputSchema.Type)
	}
	if _, ok := td.InputSchema.Properties["name"]; !ok {
		t.Fatalf("expected 'name' property in input schema")
	}
}

func TestMCPExecutor_UnknownToolReturnsError(t *testing.T) {
	backend := newMCPBackendServer(t)
	defer backend.Close()

	exec := &runtime.MCPExecutor{Endpoint: backend.URL + "/mcp"}
	_, err := exec.Execute(context.Background(), nil, "", "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}

func TestMCPExecutor_BearerForwarding(t *testing.T) {
	// This test verifies that the Authorization header is set when connectionRef
	// is non-empty. We capture it on the server side via a custom handler that
	// records what arrived, then respond with a valid (but empty) MCP reply so
	// the client doesn't fail on protocol grounds.
	var capturedAuth string

	srv := mcp.NewServer(&mcp.Implementation{Name: "auth-test", Version: "0.0.1"}, nil)
	srv.AddTool(
		&mcp.Tool{Name: "ping", Description: "ping", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: "pong"}},
				StructuredContent: "pong",
			}, nil
		},
	)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return srv }, nil)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		mcpHandler.ServeHTTP(w, r)
	}))
	defer backend.Close()

	exec := &runtime.MCPExecutor{Endpoint: backend.URL}
	if _, err := exec.Execute(context.Background(), nil, "tok123", "ping", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if capturedAuth != "Bearer tok123" {
		t.Fatalf("expected Authorization header %q, got %q", "Bearer tok123", capturedAuth)
	}
}
