package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/unitz007/open-kael/domain"
)

// MCP is the fourth way into a HostedAgent, alongside chat (listen.go),
// events (event.go), and peer dispatch (peer.go): an external MCP
// client (Claude Desktop, Claude Code, another agent framework) connects to
// one Agent and sees its Public Skills as callable MCP tools. This is not a
// new capability surface — it's the same "other agents discover Public
// Skills, never raw Tools" contract AgentDirectory already exposes for
// in-process delegation (delegate.go), re-served over MCP's wire protocol.
// A tools/call routes through the exact same HydrateSkillTools + BindSkill
// machinery every other Skill invocation uses; MCP gets no execution path
// of its own.

// AgentGraphLoader is the read-side contract RegisterMCP requires: load the
// full agent + tool + integration graph fresh from its backing store. It is
// called once per MCP session establishment (not per tool call), so the MCP
// surface always reflects the current stored state rather than a boot-time
// snapshot — an Agent or Skill created via the CRUD API after startup is
// visible on the next connection without a restart.
//
// postgres.Store satisfies this interface structurally; any other loader
// (in-memory for tests, a caching layer) only needs to match the signature.
type AgentGraphLoader interface {
	LoadAll(ctx context.Context) (agents []*domain.Agent, toolsByID map[string]*domain.ToolDefinition, identitiesByID map[string]*domain.Identity, integrationsByID map[string]*domain.Integration, err error)
}

// mcpServerVersion is the advertised server version in the MCP handshake —
// the framework's version, not any per-agent notion of one.
const mcpServerVersion = "0.1.0"

// mcpServerFor builds an *mcp.Server exposing exactly hosted's own Public
// Skills as MCP tools — private Skills and raw Tools are never exposed,
// same rule enforced everywhere else. Built fresh per session (see
// RegisterMCP's getServer), so a Skill added, removed, or made
// public/private since the last connection is reflected on the next one.
func (h *Host) mcpServerFor(hosted *HostedAgent) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "closed-kael-agent-" + hosted.Agent.ID,
		Title:   hosted.Agent.Name,
		Version: mcpServerVersion,
	}, nil)

	for _, skill := range hosted.Agent.Skills {
		if skill.Visibility != domain.SkillPublic {
			continue
		}
		tool := &mcp.Tool{
			Name:        skill.Name,
			Description: skill.Description,
			InputSchema: mcpInputSchema(skill.InputSchema),
		}
		// Only advertise an output schema when the Skill actually declares
		// one — an empty domain.Schema marshals to {"type":""}, which is not
		// valid JSON Schema and would mislead a client into expecting
		// structured output the Skill never promised.
		if skill.OutputSchema.Type != "" {
			tool.OutputSchema = skill.OutputSchema
		}
		server.AddTool(tool, h.mcpSkillHandler(hosted, skill))
	}
	return server
}

// mcpInputSchema guarantees a valid object input schema. MCP requires every
// tool's input schema to be a JSON Schema object; a Skill with no declared
// InputSchema (zero-value domain.Schema, Type == "") would otherwise
// marshal to an invalid {"type":""}, so it's replaced with a bare empty
// object schema — "this tool takes no arguments".
func mcpInputSchema(s domain.Schema) any {
	if s.Type == "" {
		return map[string]any{"type": "object"}
	}
	return s
}

// mcpSkillHandler runs one Public Skill for an MCP tools/call. It resolves
// the Skill's Tools fresh on every call (same reason actionsFor does — picks
// up any Tool/Integration change since the server was built) and invokes it
// through BindSkill, the identical path a delegate call or the Agent's own
// loop would take.
//
// An MCP call carries no conversation and no ApprovalRequester, so a Public
// Skill that binds a RequiresApproval Tool fails when that Tool runs (see
// domain.withApprovalGate) — the same fail-safe stance the cron path takes,
// and correct: there's no human on the other end of an MCP call to ask.
func (h *Host) mcpSkillHandler(hosted *HostedAgent, skill *domain.Skill) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
				return mcpErrorResult(fmt.Sprintf("invalid arguments for %q: %v", skill.Name, err)), nil
			}
		}

		tools, err := domain.HydrateSkillTools(skill, hosted.Agent, hosted.Deps.ToolsByID, hosted.Deps.IdentitiesByID, hosted.Deps.IntegrationsByID, hosted.Deps.Executors)
		if err != nil {
			return mcpErrorResult(fmt.Sprintf("hydrate skill %q: %v", skill.Name, err)), nil
		}
		action := domain.BindSkill(hosted.Agent, skill, tools)

		output, err := action.Invoke(ctx, input)
		if err != nil {
			return mcpErrorResult(fmt.Sprintf("skill %q: %v", skill.Name, err)), nil
		}
		return mcpOKResult(output), nil
	}
}

// mcpErrorResult reports a Skill-level failure as an MCP tool error
// (IsError, carried in the result) rather than a protocol error (a returned
// error) — the SDK treats a returned error as a transport/protocol fault,
// which a failed-but-well-formed Skill run is not.
func mcpErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// mcpOKResult returns the Skill's output both as StructuredContent (for a
// client that understands the tool's OutputSchema) and as a JSON text block
// (the universal fallback every MCP client can render).
func mcpOKResult(output any) *mcp.CallToolResult {
	text := fmt.Sprintf("%v", output)
	if b, err := json.Marshal(output); err == nil {
		text = string(b)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: output,
	}
}

// RegisterMCP mounts one streamable-HTTP MCP endpoint per Agent at
// /agents/{agentID}/mcp on mux, authenticated by a single shared bearer
// token (same stance as PeerHub.Handler — the token is the security
// boundary). Which Agent a request targets comes from the path; the graph
// (Agent + its Skills, Tools, Integrations) is loaded fresh from loader on
// every session establishment so newly created Agents and Skills are
// immediately visible without a restart. An unknown agent ID yields a 400
// (getServer returns nil, per the SDK contract).
//
// executors is the live ExecutorRegistry — not persisted, supplied by the
// caller from the same registry used at boot. LLMs are not injected here:
// single-tool Skills run without one; multi-tool Skills needing an LLM
// fallback will fail (same posture as the cron path) until per-agent LLM
// config is stored and loaded alongside the agent graph.
func (h *Host) RegisterMCP(mux *http.ServeMux, token string, loader AgentGraphLoader, executors *domain.ExecutorRegistry) {
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		agents, toolsByID, identitiesByID, integrationsByID, err := loader.LoadAll(r.Context())
		if err != nil {
			log.Printf("mcp: load agent graph: %v", err)
			return nil
		}
		agentID := r.PathValue("agentID")
		var target *domain.Agent
		for _, a := range agents {
			if a.ID == agentID {
				target = a
				break
			}
		}
		if target == nil {
			return nil
		}
		if h.llmFactory != nil {
			target.LLMs = h.llmFactory(target)
		}
		hosted := &HostedAgent{
			Agent: target,
			Deps: AgentDeps{
				ToolsByID:        toolsByID,
				IdentitiesByID:   identitiesByID,
				IntegrationsByID: integrationsByID,
				Executors:        executors,
			},
		}
		return h.mcpServerFor(hosted)
	}, nil)

	authed := func(w http.ResponseWriter, r *http.Request) {
		if !peerAuthorized(r, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}

	// A streamable MCP session uses POST (client->server), GET (the
	// server->client SSE stream), and DELETE (session teardown) against the
	// one endpoint, so all methods route to the same handler.
	mux.HandleFunc("/agents/{agentID}/mcp", authed)
}
