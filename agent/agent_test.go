package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/messaging"
	"github.com/unitz007/kael/tools"
	"github.com/unitz007/kael/workflow"
)

// fakeApprovalMessenger is a minimal messaging.Messenger that also
// implements messaging.ApprovalMessenger, recording which ConversationRef
// RequestApproval was actually called with.
type fakeApprovalMessenger struct {
	platform          string
	defaultRef        messaging.ConversationRef
	requestedC        messaging.ConversationRef
	requestedThreadID string
	approve           bool
}

func (f *fakeApprovalMessenger) Platform() string { return f.platform }
func (f *fakeApprovalMessenger) Send(ctx context.Context, conv messaging.ConversationRef, text string) (messaging.MessengerResponse, error) {
	return messaging.MessengerResponse{}, nil
}
func (f *fakeApprovalMessenger) Reply(ctx context.Context, to messaging.InboundMessage, text string) (messaging.MessengerResponse, error) {
	return messaging.MessengerResponse{}, nil
}
func (f *fakeApprovalMessenger) Listen(ctx context.Context, onMessage func(messaging.InboundMessage)) error {
	return nil
}
func (f *fakeApprovalMessenger) DefaultConversation() messaging.ConversationRef { return f.defaultRef }
func (f *fakeApprovalMessenger) RequestApproval(ctx context.Context, conv messaging.ConversationRef, text string) (bool, error) {
	f.requestedC = conv
	f.requestedThreadID, _ = messaging.ThreadIDFromContext(ctx)
	return f.approve, nil
}

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

func TestCallToolApprovalUsesActiveConversationForDirectTurns(t *testing.T) {
	fm := &fakeApprovalMessenger{
		platform:   "slack",
		defaultRef: messaging.ConversationRef{Platform: "slack", ChatID: "default-channel"},
		approve:    true,
	}
	a := NewAgent("test_agent", "Test Agent", "", "", stubLLM{})
	a.AddMessenger(fm)

	handlerRan := false
	tool := tools.NewToolBuilder("place_trade", "places a trade").
		RequireApproval(func(ctx context.Context, args json.RawMessage) string { return "approve?" }, 0).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) { handlerRan = true; return "ok", nil }).
		Build()

	activeConv := messaging.ConversationRef{Platform: "slack", ChatID: "active-channel"}
	ctx := messaging.WithConversation(context.Background(), activeConv)
	ctx = messaging.WithThreadID(ctx, "thread-123")

	if _, err := a.callTool(ctx, tool, json.RawMessage(`"{}"`)); err != nil {
		t.Fatalf("callTool returned an unexpected error: %v", err)
	}
	if !handlerRan {
		t.Fatalf("approval was granted but Handler never ran")
	}
	if fm.requestedC != activeConv {
		t.Fatalf("RequestApproval was called with %+v, want active conversation %+v", fm.requestedC, activeConv)
	}
	if fm.requestedThreadID != "thread-123" {
		t.Fatalf("RequestApproval lost thread id %q", fm.requestedThreadID)
	}
}

// TestCallToolApprovalIgnoresDelegatedConversation pins the safety property
// from the original approval fix: during delegation, ctx still carries the
// DELEGATING agent's own conversation, so the delegate must ask for approval in
// its own default conversation instead of inheriting that foreign one.
func TestCallToolApprovalIgnoresDelegatedConversation(t *testing.T) {
	fm := &fakeApprovalMessenger{
		platform:   "slack",
		defaultRef: messaging.ConversationRef{Platform: "slack", ChatID: "own-channel"},
		approve:    true,
	}
	a := NewAgent("test_agent", "Test Agent", "", "", stubLLM{})
	a.AddMessenger(fm)

	handlerRan := false
	tool := tools.NewToolBuilder("place_trade", "places a trade").
		RequireApproval(func(ctx context.Context, args json.RawMessage) string { return "approve?" }, 0).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) { handlerRan = true; return "ok", nil }).
		Build()

	// Simulates ctx as it would arrive mid-delegation: carrying a
	// ConversationRef that belongs to a different agent entirely.
	foreignConv := messaging.ConversationRef{Platform: "slack", ChatID: "someone-elses-channel"}
	ctx := messaging.WithConversation(context.Background(), foreignConv)
	ctx = messaging.WithDelegator(ctx, "delegating_agent")

	if _, err := a.callTool(ctx, tool, json.RawMessage(`"{}"`)); err != nil {
		t.Fatalf("callTool returned an unexpected error: %v", err)
	}
	if !handlerRan {
		t.Fatalf("approval was granted but Handler never ran")
	}
	if fm.requestedC != fm.defaultRef {
		t.Fatalf("RequestApproval was called with %+v, want this agent's own DefaultConversation() %+v — it must never inherit ctx's foreign conversation", fm.requestedC, fm.defaultRef)
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
