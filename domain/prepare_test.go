package domain_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// prepExecutor works out a size before approval (as trade sizing will) and records what it was finally sent.
type prepExecutor struct {
	ref      string
	sent     map[string]any
	fail     error
	prepared int
}

func (p *prepExecutor) Prepare(_ context.Context, _ *domain.Identity, connectionRef, _ string, in map[string]any) (map[string]any, error) {
	p.prepared++
	p.ref = connectionRef
	if p.fail != nil {
		return nil, p.fail
	}
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	out["size"] = 19000.0
	out["size_line"] = "19,000 units: 1% of £5,230"
	return out, nil
}

func (p *prepExecutor) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, in map[string]any) (any, error) {
	p.sent = in
	return map[string]any{"status": "placed", "changed": true}, nil
}

type promptRecorder struct{ prompts []string }

func (r *promptRecorder) RequestApproval(_ context.Context, _ domain.ConversationRef, prompt string, _ int) (bool, error) {
	r.prompts = append(r.prompts, prompt)
	return true, nil
}

func prepRig(t *testing.T, exec *prepExecutor, rules ...domain.ToolRule) (*domain.BoundAction, *promptRecorder, context.Context) {
	t.Helper()
	integration := &domain.Integration{ID: "int_t", Service: "prep"}
	identity := &domain.Identity{ID: "id_t", IntegrationID: integration.ID, Kind: domain.IdentityKindToken}
	registry := domain.NewExecutorRegistry()
	registry.Register("prep", exec)
	tool := &domain.ToolDefinition{ID: "tool_trade", Name: "place_trade", Description: "Places a trade", IntegrationID: integration.ID, Action: "trade",
		RequiresApproval:       true,
		ApprovalPromptTemplate: "Approve?\nSize: {{size_line}}\nNote: {{note}}\nOK",
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{
			"symbol": {Type: domain.SchemaTypeString, Description: "The pair."}, "size": {Type: domain.SchemaTypeNumber, Description: "Units."}}, Required: []string{"symbol"}}}
	bound, err := domain.HydrateTool(tool, identity, integration, registry, rules...)
	if err != nil {
		t.Fatal(err)
	}
	rec := &promptRecorder{}
	ctx := domain.WithApprovalRequester(context.Background(), rec)
	ctx = domain.WithConversation(ctx, domain.ConversationRef{ChatID: "c"})
	ctx = domain.WithConnectionRefs(ctx, map[string]string{"id_t": "ref-1"})
	return bound, rec, ctx
}

// What prepare works out is what the person approves and what the service receives; a line with nothing to show is
// left out of the approval.
func TestPrepare_TheApprovalShowsAndTheServiceGetsWhatWasPrepared(t *testing.T) {
	exec := &prepExecutor{}
	bound, rec, ctx := prepRig(t, exec)
	if _, err := bound.Invoke(ctx, map[string]any{"symbol": "EURUSD"}); err != nil {
		t.Fatal(err)
	}
	if exec.ref != "ref-1" {
		t.Errorf("prepare gets the person's own connection: %q", exec.ref)
	}
	if len(rec.prompts) != 1 || rec.prompts[0] != "Approve?\nSize: 19,000 units: 1% of £5,230\nOK" {
		t.Fatalf("the approval shows the prepared size and no placeholder: %q", rec.prompts)
	}
	if exec.sent["size"] != 19000.0 {
		t.Errorf("the service gets exactly what was approved: %v", exec.sent)
	}
}

// A prepare that cannot work it out goes back to the model in plain words: nobody is asked and nothing is sent.
func TestPrepare_AFailureStopsBeforeTheApproval(t *testing.T) {
	exec := &prepExecutor{fail: errors.New("I couldn't read your balance, so tell me the size")}
	bound, rec, ctx := prepRig(t, exec)
	_, err := bound.Invoke(ctx, map[string]any{"symbol": "EURUSD"})
	if err == nil || !strings.Contains(err.Error(), "couldn't read your balance") {
		t.Fatalf("the reason reaches the model: %v", err)
	}
	if len(rec.prompts) != 0 || exec.sent != nil {
		t.Errorf("nobody asked, nothing sent: %d %v", len(rec.prompts), exec.sent)
	}
}

// The creator's rules check the prepared call, so a rule on size holds for a size the person never typed.
func TestPrepare_TheCreatorsRulesCheckWhatWasPrepared(t *testing.T) {
	exec := &prepExecutor{}
	rule := domain.ToolRule{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 10000.0}
	bound, rec, ctx := prepRig(t, exec, rule)
	out, err := bound.Invoke(ctx, map[string]any{"symbol": "EURUSD"})
	if m, _ := out.(map[string]any); err != nil || m["status"] != "refused" {
		t.Fatalf("a prepared size over the creator's maximum is refused: %v %v", out, err)
	}
	if len(rec.prompts) != 0 || exec.sent != nil {
		t.Errorf("refused before the approval and the service: %d %v", len(rec.prompts), exec.sent)
	}
}

// An approval may show what the prepare step adds, once the tool names it; anything else it mentions must be an input.
func TestContract_AnApprovalMayShowWhatIsPrepared(t *testing.T) {
	tool := &domain.ToolDefinition{ID: "t", Name: "place_trade", Description: "Places a trade", IntegrationID: "i", Action: "trade",
		RequiresApproval: true, ApprovalPromptTemplate: "Size: {{size_line}}",
		InputSchema:      domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString, Description: "The pair."}}}}
	if err := domain.ValidateTool(tool); err == nil {
		t.Fatal("a placeholder that is neither an input nor prepared is refused")
	}
	tool.Prepared = []string{"size_line"}
	if err := domain.ValidateTool(tool); err != nil {
		t.Fatalf("a prepared value may be shown: %v", err)
	}
	if _, ok := tool.Spec().Input.Properties["size_line"]; ok {
		t.Error("the model never sees a prepared value as an input")
	}
}
