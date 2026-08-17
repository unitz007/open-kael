package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/tools"
	"github.com/unitz007/kael/workflow"
)

// stubLLM satisfies llm.LLM without ever being called — workflowToolset is
// pure toolset composition, no loop involved.
type stubLLM struct{}

func (stubLLM) Call(messages []llm.Message, toolset []*tools.ToolSpec) (*llm.Response, error) {
	panic("stubLLM: Call should not be invoked by this test")
}

func hasToolNamed(toolset []*tools.ToolSpec, name string) bool {
	for _, t := range toolset {
		if t.Name == name {
			return true
		}
	}
	return false
}

// TestWorkflowToolsetAlwaysIncludesApprovalGatedTools pins the exact safety
// property this package's whole approval mechanism depends on: a tool
// added via AddTool — including one marked RequiresApproval, like Kael's
// place_trade/submit_job_application — must remain in a workflow's toolset
// regardless of whether messagingTools is supplied. This used to be an
// emergent side effect of includeUserReply's base-computation shape
// (base := a.Tools, only ever widened, never narrowed); this test exists
// so that guarantee stays enforced even if runWorkflow's composition is
// refactored again later.
func TestWorkflowToolsetAlwaysIncludesApprovalGatedTools(t *testing.T) {
	a := NewAgent("test_agent", "Test Agent", "", "", stubLLM{})
	a.AddTool(tools.NewToolBuilder("place_trade", "places a trade").
		RequireApproval(func(ctx context.Context, args json.RawMessage) string { return "approve?" }, 0).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil }).
		Build())

	wf := &workflow.Workflow{ID: "place_trade_wf", Name: "Place Trade Workflow"} // no Tools map of its own, mirrors placeTradeWorkflow in Kael

	for _, tc := range []struct {
		name           string
		messagingTools []*tools.ToolSpec
	}{
		{"with messaging tools (live/webhook/cron)", []*tools.ToolSpec{{Name: "send_message"}}},
		{"without messaging tools (delegated)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toolset := a.workflowToolset(wf, tc.messagingTools)
			if !hasToolNamed(toolset, "place_trade") {
				t.Fatalf("workflowToolset(messagingTools=%v) dropped place_trade — an approval-gated tool must always be reachable", tc.messagingTools)
			}
			place, _ := findTool(toolset, "place_trade")
			if !place.RequiresApproval {
				t.Fatalf("place_trade lost its RequiresApproval flag in the composed toolset")
			}
		})
	}
}

func findTool(toolset []*tools.ToolSpec, name string) (*tools.ToolSpec, bool) {
	for _, t := range toolset {
		if t.Name == name {
			return t, true
		}
	}
	return nil, false
}
