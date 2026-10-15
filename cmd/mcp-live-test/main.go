// Command mcp-live-test exercises MCPExecutor against a real github-mcp-server.
//
// Two tests run back-to-back:
//
//  1. Direct stdio: spawns github-mcp-server via CommandTransport, lists tools,
//     calls get_me and list_branches on unitz007/open-kael.
//  2. MCPExecutor via HTTP bridge: spawns github-mcp-server in stdio mode,
//     registers its tools on a local streamable-HTTP bridge server, then
//     exercises MCPExecutor.Seed and MCPExecutor.Execute against that bridge.
//     This avoids the HTTP-mode PAT format check (the subprocess uses the
//     env-var token which the proxy replaces transparently).
//
// Usage:
//
//	go run ./cmd/mcp-live-test -bin /tmp/github-mcp-server-bin
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

const (
	toolsets  = "context,repos,users"
	testOwner = "unitz007"
	testRepo  = "open-kael"
)

func main() {
	binFlag := flag.String("bin", "/tmp/github-mcp-server-bin", "path to github-mcp-server binary")
	flag.Parse()

	token := os.Getenv("GITHUB_PERSONAL_ACCESS_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		log.Fatal("set GITHUB_PERSONAL_ACCESS_TOKEN or GITHUB_TOKEN")
	}
	if _, err := os.Stat(*binFlag); err != nil {
		log.Fatalf("binary not found at %q: %v", *binFlag, err)
	}

	ctx := context.Background()

	fmt.Println("=== 1. Direct stdio connection ===")
	if err := directStdioTest(ctx, *binFlag, token); err != nil {
		log.Fatalf("stdio: %v", err)
	}

	fmt.Println("\n=== 2. MCPExecutor via HTTP bridge ===")
	if err := mcpExecutorTest(ctx, *binFlag, token); err != nil {
		log.Fatalf("MCPExecutor: %v", err)
	}
}

// directStdioTest uses mcp.CommandTransport to talk directly to the subprocess.
func directStdioTest(ctx context.Context, bin, token string) error {
	sess, cleanup, err := stdioSession(ctx, bin, token)
	if err != nil {
		return err
	}
	defer cleanup()

	listResult, err := sess.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	fmt.Printf("tools available (%d):\n", len(listResult.Tools))
	for _, t := range listResult.Tools {
		fmt.Printf("  • %-40s %s\n", t.Name, shortDesc(t.Description))
	}

	fmt.Println("\ncalling get_me…")
	callAndPrint(ctx, sess, "get_me", map[string]any{})

	fmt.Printf("\ncalling list_branches(%s/%s)…\n", testOwner, testRepo)
	callAndPrint(ctx, sess, "list_branches",
		map[string]any{"owner": testOwner, "repo": testRepo})

	return nil
}

// mcpExecutorTest builds a local HTTP bridge server backed by a github-mcp-server
// subprocess, then exercises MCPExecutor.Seed and MCPExecutor.Execute against it.
func mcpExecutorTest(ctx context.Context, bin, token string) error {
	// Connect a fresh subprocess session — the bridge server calls through it.
	subSess, cleanup, err := stdioSession(ctx, bin, token)
	if err != nil {
		return fmt.Errorf("start subprocess: %w", err)
	}
	defer cleanup()

	// Discover all tools from the subprocess and register forwarding handlers on
	// a local mcp.Server so MCPExecutor (which needs HTTP) can reach them.
	bridgeSrv := mcp.NewServer(&mcp.Implementation{Name: "github-bridge", Version: "0.0.1"}, nil)

	listResult, err := subSess.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools from subprocess: %w", err)
	}
	for _, t := range listResult.Tools {
		t := t // capture per loop iteration
		bridgeSrv.AddTool(t, func(callCtx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &args)
			}
			return subSess.CallTool(callCtx, &mcp.CallToolParams{
				Name:      t.Name,
				Arguments: args,
			})
		})
	}
	fmt.Printf("bridge registered %d tools from subprocess\n", len(listResult.Tools))

	// Serve the bridge server on a free local port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return bridgeSrv }, nil)
	httpSrv := httptest.NewUnstartedServer(handler)
	httpSrv.Listener = ln
	httpSrv.Start()
	defer httpSrv.Close()

	mcpEndpoint := httpSrv.URL + "/mcp"
	fmt.Printf("bridge listening at %s\n\n", mcpEndpoint)

	exec := &runtime.MCPExecutor{Endpoint: mcpEndpoint}

	// Seed: discover tools via MCPExecutor.
	fmt.Println("Seed (tools/list via MCPExecutor):")
	var tools []*domain.ToolDefinition
	if err := exec.Seed(ctx, "github-mcp", func(_ context.Context, td *domain.ToolDefinition) error {
		tools = append(tools, td)
		return nil
	}); err != nil {
		return fmt.Errorf("Seed: %w", err)
	}
	fmt.Printf("  %d tools discovered\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  • %-40s action=%s  schema_type=%s\n",
			t.Name, t.Action, t.InputSchema.Type)
	}

	// Execute: call get_me via MCPExecutor.
	fmt.Println("\nExecute(get_me) via MCPExecutor:")
	out, err := exec.Execute(ctx, nil, "", "get_me", map[string]any{})
	if err != nil {
		return fmt.Errorf("Execute get_me: %w", err)
	}
	printJSON(out)

	// Execute: call list_branches via MCPExecutor.
	fmt.Printf("\nExecute(list_branches, %s/%s) via MCPExecutor:\n", testOwner, testRepo)
	out, err = exec.Execute(ctx, nil, "", "list_branches",
		map[string]any{"owner": testOwner, "repo": testRepo})
	if err != nil {
		return fmt.Errorf("Execute list_branches: %w", err)
	}
	printJSON(out)
	return nil
}

// stdioSession spawns a github-mcp-server subprocess and returns an mcp.ClientSession.
func stdioSession(ctx context.Context, bin, token string) (*mcp.ClientSession, func(), error) {
	cmd := exec.CommandContext(ctx, bin, "stdio", "--read-only", "--toolsets="+toolsets)
	cmd.Env = append(os.Environ(), "GITHUB_PERSONAL_ACCESS_TOKEN="+token)

	client := mcp.NewClient(&mcp.Implementation{Name: "live-test", Version: "0.0.1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect subprocess: %w", err)
	}
	return sess, func() { sess.Close() }, nil
}

func callAndPrint(ctx context.Context, sess *mcp.ClientSession, name string, args map[string]any) {
	result, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}
	if result.IsError {
		for _, c := range result.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				fmt.Println("  ERROR:", tc.Text)
			}
		}
		return
	}
	if result.StructuredContent != nil {
		printJSON(result.StructuredContent)
		return
	}
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s := tc.Text
			if len(s) > 600 {
				s = s[:597] + "…"
			}
			fmt.Println(" ", s)
		}
	}
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "  ", "  ")
	s := string(b)
	if len(s) > 600 {
		s = s[:597] + "…"
	}
	fmt.Println(" ", s)
}

func shortDesc(s string) string {
	if len(s) > 60 {
		return s[:57] + "…"
	}
	return s
}
