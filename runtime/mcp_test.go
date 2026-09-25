package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// bearerRoundTripper injects a fixed Authorization header on every request
// — the MCP StreamableClientTransport has no auth field of its own, so auth
// rides on the underlying http.Client.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// mcpTestEnv is the complete wiring needed to call RegisterMCP in tests —
// a Host (receiver), an AgentGraphLoader (in-memory, no DB), and the
// ExecutorRegistry. Using a loader rather than the Host's own agent map
// exercises the same path production uses (RegisterMCP calls loader.LoadAll
// per session, not h.Get).
type mcpTestEnv struct {
	host      *runtime.Host
	loader    runtime.AgentGraphLoader
	executors *domain.ExecutorRegistry
}

func (e *mcpTestEnv) register(mux *http.ServeMux, token string) {
	e.host.RegisterMCP(mux, token, e.loader, e.executors)
}

// staticLoader is an in-memory AgentGraphLoader for tests — returns the
// same fixed data on every LoadAll call, mirroring what postgres.Store does
// in production.
type staticLoader struct {
	agents           []*domain.Agent
	toolsByID        map[string]*domain.ToolDefinition
	identitiesByID   map[string]*domain.Identity
	integrationsByID map[string]*domain.Integration
}

func (l *staticLoader) LoadAll(_ context.Context) ([]*domain.Agent, map[string]*domain.ToolDefinition, map[string]*domain.Identity, map[string]*domain.Integration, error) {
	return l.agents, l.toolsByID, l.identitiesByID, l.integrationsByID, nil
}

// newMCPTestEnv builds a test environment with one agent "a1" that owns a
// public single-tool Skill (echo, backed by fakeExecutor) and a private
// Skill (secret) that must never be exposed over MCP.
func newMCPTestEnv() *mcpTestEnv {
	integration := &domain.Integration{ID: "int1", Service: "fake"}
	echoTool := &domain.ToolDefinition{
		ID:            "t_echo",
		Name:          "echo",
		IntegrationID: "int1",
		Action:        "fake.echo",
		InputSchema: domain.Schema{
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"text": {Type: domain.SchemaTypeString}},
			Required:   []string{"text"},
		},
	}
	executors := domain.NewExecutorRegistry()
	executors.Register("fake", &fakeExecutor{})

	agent := &domain.Agent{
		ID:   "a1",
		Name: "Agent One",
		Skills: []*domain.Skill{
			{
				ID:          "s_echo",
				Name:        "echo",
				Description: "Echo the input back.",
				Visibility:  domain.SkillPublic,
				InputSchema: domain.Schema{
					Type:       domain.SchemaTypeObject,
					Properties: map[string]domain.Schema{"text": {Type: domain.SchemaTypeString}},
					Required:   []string{"text"},
				},
				OutputSchema: domain.Schema{Type: domain.SchemaTypeString},
				Tools:        []domain.ToolBinding{{ToolID: "t_echo"}},
			},
			{
				ID:         "s_secret",
				Name:       "secret",
				Visibility: domain.SkillPrivate,
				Tools:      []domain.ToolBinding{{ToolID: "t_echo"}},
			},
		},
	}

	return &mcpTestEnv{
		host: runtime.NewHost(),
		loader: &staticLoader{
			agents:           []*domain.Agent{agent},
			toolsByID:        map[string]*domain.ToolDefinition{"t_echo": echoTool},
			integrationsByID: map[string]*domain.Integration{"int1": integration},
		},
		executors: executors,
	}
}

// connectMCP dials the MCP endpoint for agentID through the test server,
// authenticating with token. The caller closes the returned session.
func connectMCP(t *testing.T, srv *httptest.Server, agentID, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/agents/" + agentID + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}},
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return session
}

func TestMCP_ListsOnlyPublicSkills(t *testing.T) {
	mux := http.NewServeMux()
	newMCPTestEnv().register(mux, "secret-token")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	session := connectMCP(t, srv, "a1", "secret-token")
	defer session.Close()

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(result.Tools) != 1 {
		t.Fatalf("expected exactly 1 tool (the public Skill), got %d: %+v", len(result.Tools), result.Tools)
	}
	if result.Tools[0].Name != "echo" {
		t.Fatalf("expected the public Skill %q, got %q", "echo", result.Tools[0].Name)
	}
}

func TestMCP_CallPublicSkill(t *testing.T) {
	mux := http.NewServeMux()
	newMCPTestEnv().register(mux, "secret-token")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	session := connectMCP(t, srv, "a1", "secret-token")
	defer session.Close()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result.Content)
	}
	// The single-tool Skill passes straight through to fakeExecutor, which
	// returns "ok" — surfaced as the structured result.
	if got, ok := result.StructuredContent.(string); !ok || got != "ok" {
		t.Fatalf("unexpected structured content: %#v", result.StructuredContent)
	}
}

func TestMCP_RejectsMissingToken(t *testing.T) {
	mux := http.NewServeMux()
	newMCPTestEnv().register(mux, "secret-token")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/agents/a1/mcp"}
	if _, err := client.Connect(context.Background(), transport, nil); err == nil {
		t.Fatal("expected connect to fail without a bearer token")
	}
}

func TestMCP_UnknownAgentIsRejected(t *testing.T) {
	mux := http.NewServeMux()
	newMCPTestEnv().register(mux, "secret-token")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/agents/does-not-exist/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: "secret-token", base: http.DefaultTransport}},
	}
	if _, err := client.Connect(context.Background(), transport, nil); err == nil {
		t.Fatal("expected connect to fail for an unknown agent")
	}
}
