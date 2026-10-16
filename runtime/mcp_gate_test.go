package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// gateBackend is a real MCP server with one tool of each kind a remote server
// can declare, counting how many times each is actually called.
type gateBackend struct {
	srv                     *httptest.Server
	lookups, changes, wipes atomic.Int32
}

func newGateBackend(t *testing.T) *gateBackend {
	t.Helper()
	b := &gateBackend{}
	s := mcp.NewServer(&mcp.Implementation{Name: "gate-backend", Version: "0.0.1"}, nil)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"note":   map[string]any{"type": "string"},
			"amount": map[string]any{"type": "number"},
		},
		"required": []string{"amount"},
	}
	ok := func(c *atomic.Int32) mcp.ToolHandler {
		return func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			c.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
		}
	}
	destructive := true
	s.AddTool(&mcp.Tool{Name: "lookup", Description: "Look something up. Never changes anything.", InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, ok(&b.lookups))
	s.AddTool(&mcp.Tool{Name: "change", Description: "Change something. It has side effects.", InputSchema: schema}, ok(&b.changes))
	s.AddTool(&mcp.Tool{Name: "wipe", Description: "Delete everything.", InputSchema: schema,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}}, ok(&b.wipes))

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *gateBackend) seed(t *testing.T) (*runtime.MCPExecutor, map[string]*domain.ToolDefinition) {
	t.Helper()
	exec := &runtime.MCPExecutor{Endpoint: b.srv.URL + "/mcp"}
	defs := map[string]*domain.ToolDefinition{}
	if err := exec.Seed(context.Background(), "remote", func(_ context.Context, d *domain.ToolDefinition) error {
		defs[d.Name] = d
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return exec, defs
}

func TestMCPSeed_OnlyServerDeclaredReadOnlyToolsRunUnattended(t *testing.T) {
	_, defs := newGateBackend(t).seed(t)
	if len(defs) != 3 {
		t.Fatalf("want 3 tools, got %d", len(defs))
	}
	if d := defs["lookup"]; !d.ReadOnly || d.RequiresApproval {
		t.Errorf("a tool the server says is read-only should run unattended: %+v", d)
	}
	for _, name := range []string{"change", "wipe"} {
		d := defs[name]
		if d.ReadOnly || !d.RequiresApproval || d.ApprovalTimeoutSeconds == 0 {
			t.Errorf("%s must require approval (no read-only proof): %+v", name, d)
		}
	}
}

func TestMCPSeed_ApprovalPromptShowsTheExactArguments(t *testing.T) {
	_, defs := newGateBackend(t).seed(t)
	tmpl := defs["change"].ApprovalPromptTemplate
	// Required arguments come first, then the others.
	if strings.Index(tmpl, "amount: {{amount}}") < 0 || strings.Index(tmpl, "note: {{note}}") < strings.Index(tmpl, "amount: {{amount}}") {
		t.Errorf("template should list required args first: %q", tmpl)
	}
	prompt := defs["change"].RenderApprovalPrompt(map[string]any{"amount": 250.5, "note": "refund"})
	for _, want := range []string{`Approve "change"?`, "Change something.", "amount: 250.5", "note: refund", "outside service"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "{{") {
		t.Errorf("unfilled placeholder left in prompt:\n%s", prompt)
	}
}

// runTool drives one agent turn that calls the single-tool skill for `tool`,
// with a messenger that either approves or can't ask at all.
func runTool(t *testing.T, mcpExec *runtime.MCPExecutor, def *domain.ToolDefinition, messenger domain.Executor) {
	t.Helper()
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "use_skill", Arguments: map[string]any{"amount": 5.0, "note": "x"}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
	}}
	skill := &domain.Skill{ID: "s1", Name: "use_skill", AgentID: "a1", Instructions: "use the tool", Tools: []domain.ToolBinding{{ToolID: def.ID}}}
	agent := &domain.Agent{ID: "a1", Instructions: "help", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}}

	execs := domain.NewExecutorRegistry()
	execs.Register("slack", messenger)
	execs.Register("remote", mcpExec)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.ToolsByID = map[string]*domain.ToolDefinition{def.ID: def}
		d.IntegrationsByID["remote"] = &domain.Integration{ID: "remote", Service: "remote"}
		d.Executors = execs
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	if _, err := host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1"}, "go"); err != nil {
		t.Fatalf("handle turn: %v", err)
	}
}

func TestMCPTool_ChangingToolNeverRunsWithoutAnApprover(t *testing.T) {
	b := newGateBackend(t)
	exec, defs := b.seed(t)
	runTool(t, exec, defs["change"], &fakeExecutor{}) // a messenger that cannot ask
	runTool(t, exec, defs["wipe"], &fakeExecutor{})
	if b.changes.Load() != 0 || b.wipes.Load() != 0 {
		t.Fatalf("a tool that can change things ran with nobody to approve it: changes=%d wipes=%d", b.changes.Load(), b.wipes.Load())
	}
}

func TestMCPTool_RejectedApprovalNeverRuns(t *testing.T) {
	b := newGateBackend(t)
	exec, defs := b.seed(t)
	runTool(t, exec, defs["change"], &approvingExecutor{approve: false})
	if b.changes.Load() != 0 {
		t.Fatalf("a rejected tool ran anyway")
	}
}

func TestMCPTool_RunsOnceApproved(t *testing.T) {
	b := newGateBackend(t)
	exec, defs := b.seed(t)
	runTool(t, exec, defs["change"], &approvingExecutor{approve: true})
	if b.changes.Load() != 1 {
		t.Fatalf("an approved tool should run exactly once, ran %d times", b.changes.Load())
	}
}

func TestMCPTool_ReadOnlyRunsWithoutAsking(t *testing.T) {
	b := newGateBackend(t)
	exec, defs := b.seed(t)
	runTool(t, exec, defs["lookup"], &fakeExecutor{}) // nobody can approve, and nobody needs to
	if b.lookups.Load() != 1 {
		t.Fatalf("a read-only tool should run without approval, ran %d times", b.lookups.Load())
	}
}
