package domain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestToolSpec_IsTheContractAsData(t *testing.T) {
	def := &domain.ToolDefinition{
		ID: "svc.get_quote", Name: "Get quote", FunctionName: "get_quote", IntegrationID: "svc", Action: "q",
		Description: "Shows a price.", Instructions: "Ask which symbol.", ReadOnly: true, MaxCalls: 3,
		RequiresApproval: true, ApprovalPromptTemplate: "Look up {{symbol}}?", ApprovalTimeoutSeconds: 45,
		InputSchema:  obj([]string{"symbol"}, map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString}}),
		OutputSchema: domain.Schema{Type: domain.SchemaTypeObject},
	}
	s := def.Spec()
	if s.ID != "svc.get_quote" || s.SourceID != "svc" || s.Name != "get_quote" || s.Title != "Get quote" || s.MaxCalls != 3 ||
		s.Description != "Shows a price." || s.Instructions != "Ask which symbol." {
		t.Fatalf("identity and wording: %+v", s)
	}
	if s.Effect != domain.EffectReads {
		t.Fatalf("effect: %v", s.Effect)
	}
	if !s.Approval.Required || s.Approval.PromptTemplate != "Look up {{symbol}}?" || s.Approval.TimeoutSeconds != 45 {
		t.Fatalf("approval: %+v", s.Approval)
	}
	if len(s.Input.Required) != 1 || s.Input.Properties["symbol"].Type != domain.SchemaTypeString {
		t.Fatalf("input: %+v", s.Input)
	}
	def.ReadOnly = false
	if def.Spec().Effect != domain.EffectChanges {
		t.Fatal("not read-only means a change")
	}
}

// Whatever holds a Tool can only run it through its gates.
func TestBindTool_TheGatesAreThePartOfTheTool(t *testing.T) {
	def := &domain.ToolDefinition{
		ID: "svc.place_trade", Name: "place_trade", IntegrationID: "svc", Action: "place", Description: "Places a trade.",
		RequiresApproval: true, ApprovalPromptTemplate: "Trade {{symbol}}?", ApprovalTimeoutSeconds: 30,
		InputSchema: obj([]string{"symbol"}, map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString}}),
	}
	exec := &recordingExecutor{}
	reg := domain.NewExecutorRegistry()
	reg.Register("svc", exec)
	var tool domain.Tool // used only through the interface
	tool, err := domain.BindTool(def, nil, &domain.Integration{ID: "svc", Service: "svc"}, reg)
	if err != nil {
		t.Fatal(err)
	}
	approver := &countingApprover{}
	ctx := domain.WithApprovalRequester(domain.WithConversation(context.Background(), domain.ConversationRef{Provider: "x", ChatID: "c"}), approver)

	if _, err := tool.Invoke(ctx, map[string]any{}); err == nil || !strings.Contains(err.Error(), `"symbol" is required`) {
		t.Fatalf("input gate: %v", err)
	}
	if approver.asked != 0 || len(exec.calls) != 0 {
		t.Fatal("an invalid call reaches neither approval nor the executor")
	}
	if _, err := tool.Invoke(context.Background(), map[string]any{"symbol": "EURUSD"}); err == nil {
		t.Fatal("approval gate: a tool that needs approval must not run with nobody to ask")
	}
	if len(exec.calls) != 0 {
		t.Fatal("it must not have run unapproved")
	}
	if out, err := tool.Invoke(ctx, map[string]any{"symbol": "EURUSD"}); err != nil || out != "done" || approver.asked != 1 || len(exec.calls) != 1 {
		t.Fatalf("approved call: %v %v asked=%d", out, err, approver.asked)
	}
}

func TestBindTool_RefusesWhatCannotRun(t *testing.T) {
	def := goodTool()
	if _, err := domain.BindTool(def, nil, &domain.Integration{ID: "other"}, domain.NewExecutorRegistry()); err == nil {
		t.Fatal("a tool can't be bound to another integration")
	}
	if _, err := domain.BindTool(def, nil, &domain.Integration{ID: "svc", Service: "svc"}, domain.NewExecutorRegistry()); err == nil {
		t.Fatal("a tool with no executor can't be bound")
	}
}

// The loop and the model clients run on BoundAction; the adapter adds nothing to a Tool.
func TestActionOf_CarriesTheToolAcross(t *testing.T) {
	def := goodTool()
	reg := domain.NewExecutorRegistry()
	exec := &recordingExecutor{}
	reg.Register("svc", exec)
	tool, _ := domain.BindTool(def, nil, &domain.Integration{ID: "svc", Service: "svc"}, reg)
	a := domain.ActionOf(tool)
	if a.Spec.Name != "get_quote" || !a.Spec.ReadOnly || a.Spec.Description != def.Description || a.Spec.InputSchema.Required[0] != "symbol" {
		t.Fatalf("spec: %+v", a.Spec)
	}
	if _, err := a.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatal("the action runs the tool, gates and all")
	}
	if _, err := a.Invoke(context.Background(), map[string]any{"symbol": "X"}); err != nil || len(exec.calls) != 1 {
		t.Fatalf("a valid call: %v", err)
	}

	// Anything that implements Tool can be run by the loop, not just bound definitions.
	custom := fakeTool{}
	if ca := domain.ActionOf(custom); ca.Spec.Name != "fake" || ca.Spec.ReadOnly {
		t.Fatalf("a different Tool implementation: %+v", ca.Spec)
	}
}

type fakeTool struct{}

func (fakeTool) Spec() domain.ToolSpec {
	return domain.ToolSpec{ID: "f", Name: "fake", Effect: domain.EffectChanges}
}
func (fakeTool) Invoke(context.Context, map[string]any) (any, error) { return nil, nil }
