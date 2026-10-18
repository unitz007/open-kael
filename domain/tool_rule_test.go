package domain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// ruleExecutor remembers every input it was asked to run with.
type ruleExecutor struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (r *ruleExecutor) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, input map[string]any) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, input)
	return map[string]any{"status": "placed", "changed": true, "message": "placed"}, nil
}
func (r *ruleExecutor) n() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.calls) }

func ruleTradeTool() *domain.ToolDefinition {
	return &domain.ToolDefinition{
		ID: "tool_trade", Name: "place_trade", IntegrationID: "int_1", Action: "trade.place",
		Description: "Places a trade.",
		InputSchema: domain.Schema{
			Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{
				"symbol":     {Type: domain.SchemaTypeString, Description: "The pair."},
				"size":       {Type: domain.SchemaTypeInteger, Description: "Units."},
				"min_rr":     {Type: domain.SchemaTypeNumber, Description: "Smallest reward to risk."},
				"order_type": {Type: domain.SchemaTypeString, Enum: []string{"limit", "market"}},
			},
			Required: []string{"symbol", "size", "min_rr"},
		},
	}
}

func bindWith(t *testing.T, exec *ruleExecutor, rules ...domain.ToolRule) *domain.BoundAction {
	t.Helper()
	reg := domain.NewExecutorRegistry()
	reg.Register("trade", exec)
	a, err := domain.HydrateTool(ruleTradeTool(), nil, &domain.Integration{ID: "int_1", Service: "trade"}, reg, rules...)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func norm(t *testing.T, r domain.ToolRule) domain.ToolRule {
	t.Helper()
	n, err := domain.NormalizeRule(r, ruleTradeTool())
	if err != nil {
		t.Fatalf("%+v: %v", r, err)
	}
	return n
}

func refused(out any) (string, bool) {
	m, ok := out.(map[string]any)
	if !ok || m["status"] != "refused" {
		return "", false
	}
	msg, _ := m["message"].(string)
	changed, _ := m["changed"].(bool)
	return msg, !changed
}

// ── the gate ──────────────────────────────────────────────────────────────────

func TestRules_AMaximumRefusesBeforeTheServiceSeesTheCallAndAllowsTheLimitItself(t *testing.T) {
	exec := &ruleExecutor{}
	a := bindWith(t, exec, norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 100000.0}))
	ctx := context.Background()

	out, err := a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 100001.0, "min_rr": 2.0})
	msg, ok := refused(out)
	if err != nil || !ok || !strings.Contains(msg, "more than the most allowed for size (100000)") || !strings.Contains(msg, "nothing was done") {
		t.Fatalf("a call over the maximum is refused in plain words with changed=false: %v %v", out, err)
	}
	if exec.n() != 0 {
		t.Fatal("the service must never see a refused call")
	}
	if done, _, _ := domain.OutcomeOf(out, err); done {
		t.Error("a refusal must not count as done")
	}
	if _, err := a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 100000.0, "min_rr": 2.0}); err != nil || exec.n() != 1 {
		t.Fatalf("exactly the maximum is allowed: calls=%d err=%v", exec.n(), err)
	}
}

func TestRules_AMinimumOneOfAndNoneOf(t *testing.T) {
	exec := &ruleExecutor{}
	a := bindWith(t, exec,
		norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "min_rr", Kind: domain.RuleMin, Value: 1.5}),
		norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "symbol", Kind: domain.RuleOneOf, Value: []any{"EURUSD", "GBPUSD"}}),
		norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "order_type", Kind: domain.RuleNoneOf, Value: []any{"market"}}),
	)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		in   map[string]any
		want string
	}{
		"below the minimum":   {map[string]any{"symbol": "EURUSD", "size": 10.0, "min_rr": 1.0}, "less than the least allowed for min rr (1.5)"},
		"a symbol not listed": {map[string]any{"symbol": "USDJPY", "size": 10.0, "min_rr": 2.0}, "Only these are allowed for symbol: EURUSD, GBPUSD"},
		"a blocked value":     {map[string]any{"symbol": "EURUSD", "size": 10.0, "min_rr": 2.0, "order_type": "market"}, "Market isn't allowed for order type"},
	} {
		out, _ := a.Invoke(ctx, tc.in)
		if msg, ok := refused(out); !ok || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, out)
		}
	}
	if exec.n() != 0 {
		t.Fatalf("nothing reached the service: %d", exec.n())
	}
	// Symbols are compared without regard to case; everything within the rules goes through.
	if _, err := a.Invoke(ctx, map[string]any{"symbol": "eurusd", "size": 10.0, "min_rr": 2.0, "order_type": "limit"}); err != nil || exec.n() != 1 {
		t.Fatalf("a call within every rule runs: %d %v", exec.n(), err)
	}
}

func TestRules_AFixedValueIsHiddenFromTheModelAndAlwaysSent(t *testing.T) {
	exec := &ruleExecutor{}
	a := bindWith(t, exec, norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "min_rr", Kind: domain.RuleFixed, Value: 2.0}))

	if _, shown := a.Spec.InputSchema.Properties["min_rr"]; shown {
		t.Error("the model must not see an input the creator fixed")
	}
	for _, r := range a.Spec.InputSchema.Required {
		if r == "min_rr" {
			t.Error("and it must not be required of the model")
		}
	}
	// The model omits it: the tool still gets it, and its own required check is satisfied.
	if _, err := a.Invoke(context.Background(), map[string]any{"symbol": "EURUSD", "size": 10.0}); err != nil {
		t.Fatal(err)
	}
	// The model tries to set it anyway: the creator's value wins.
	if _, err := a.Invoke(context.Background(), map[string]any{"symbol": "GBPUSD", "size": 10.0, "min_rr": 0.0}); err != nil {
		t.Fatal(err)
	}
	for i, c := range exec.calls {
		if c["min_rr"] != 2.0 {
			t.Errorf("call %d: the fixed value must reach the service, got %v", i, c["min_rr"])
		}
	}
}

func TestRules_ADefaultFillsInWhatIsMissingAndNeverOverridesWhatWasGiven(t *testing.T) {
	exec := &ruleExecutor{}
	a := bindWith(t, exec, norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "order_type", Kind: domain.RuleDefault, Value: "limit"}))
	if d := a.Spec.InputSchema.Properties["order_type"].Description; !strings.Contains(d, "limit is used") {
		t.Errorf("the model is told what the default is: %q", d)
	}
	ctx := context.Background()
	a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 1.0, "min_rr": 2.0})
	a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 1.0, "min_rr": 2.0, "order_type": "market"})
	if exec.calls[0]["order_type"] != "limit" || exec.calls[1]["order_type"] != "market" {
		t.Errorf("default when absent, the model's own value when given: %v %v", exec.calls[0], exec.calls[1])
	}
}

func TestRules_TheModelIsToldTheLimitsUpFront(t *testing.T) {
	a := bindWith(t, &ruleExecutor{},
		norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 50000.0}),
		norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "symbol", Kind: domain.RuleOneOf, Value: []any{"GBPUSD", "EURUSD"}}),
	)
	p := a.Spec.InputSchema.Properties
	if !strings.Contains(p["size"].Description, "at most 50000") || !strings.Contains(p["symbol"].Description, "only EURUSD, GBPUSD") {
		t.Errorf("limits are stated in what the model sees: %q / %q", p["size"].Description, p["symbol"].Description)
	}
	// What the model sees is a copy: the stored tool is untouched.
	if strings.Contains(ruleTradeTool().InputSchema.Properties["size"].Description, "50000") {
		t.Error("the tool definition itself must not change")
	}
}

func TestRules_TheCreatorsOwnWordingIsUsedVerbatim(t *testing.T) {
	exec := &ruleExecutor{}
	a := bindWith(t, exec, norm(t, domain.ToolRule{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 10.0, Message: "Trades over 10 units need a manager."}))
	out, _ := a.Invoke(context.Background(), map[string]any{"symbol": "EURUSD", "size": 11.0, "min_rr": 2.0})
	if msg, ok := refused(out); !ok || msg != "Trades over 10 units need a manager." {
		t.Errorf("verbatim: %v", out)
	}
}

// A refused call must not bother the person: no approval prompt, and the one-shot guard stays free.
func TestRules_ARefusedCallNeverReachesTheApprovalPromptAndLeavesTheToolFree(t *testing.T) {
	tool := ruleTradeTool()
	tool.RequiresApproval = true
	exec := &ruleExecutor{}
	reg := domain.NewExecutorRegistry()
	reg.Register("trade", exec)
	rule, err := domain.NormalizeRule(domain.ToolRule{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 100.0}, tool)
	if err != nil {
		t.Fatal(err)
	}
	a, err := domain.HydrateTool(tool, nil, &domain.Integration{ID: "int_1", Service: "trade"}, reg, rule)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	ctx := domain.WithApprovalRequester(domain.WithConversation(context.Background(), domain.ConversationRef{Provider: "x", ChatID: "c"}),
		approverFunc(func() bool { asked++; return true }))

	if out, _ := a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 500.0, "min_rr": 2.0}); out == nil {
		t.Fatal("expected a refusal")
	}
	if asked != 0 || exec.n() != 0 {
		t.Fatalf("the person is not asked about a call that was never going to run: asked=%d ran=%d", asked, exec.n())
	}
	if _, err := a.Invoke(ctx, map[string]any{"symbol": "EURUSD", "size": 50.0, "min_rr": 2.0}); err != nil || asked != 1 || exec.n() != 1 {
		t.Fatalf("a call within the rules still goes through approval and runs: asked=%d ran=%d err=%v", asked, exec.n(), err)
	}
}

type approverFunc func() bool

func (f approverFunc) RequestApproval(context.Context, domain.ConversationRef, string, int) (bool, error) {
	return f(), nil
}

func TestRules_OnlyTheRulesForThisToolApply(t *testing.T) {
	a := &domain.Agent{Rules: []domain.ToolRule{
		{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 1.0},
		{ToolID: "tool_other", Input: "x", Kind: domain.RuleMax, Value: 2.0},
	}}
	if got := a.RulesFor("tool_trade"); len(got) != 1 || got[0].Input != "size" {
		t.Errorf("rules for one tool: %+v", got)
	}
	var nilAgent *domain.Agent
	if nilAgent.RulesFor("tool_trade") != nil {
		t.Error("no agent, no rules")
	}
}

// ── checking a rule against its tool ─────────────────────────────────────────

func TestNormalizeRule_RefusesWhatCannotWorkInWordsTheCreatorCanRead(t *testing.T) {
	for name, tc := range map[string]struct {
		r    domain.ToolRule
		want string
	}{
		"unknown input":    {domain.ToolRule{Input: "nope", Kind: domain.RuleMax, Value: 1.0}, "isn't something this tool takes"},
		"max on text":      {domain.ToolRule{Input: "symbol", Kind: domain.RuleMax, Value: 1.0}, "isn't a number"},
		"max not a number": {domain.ToolRule{Input: "size", Kind: domain.RuleMax, Value: "lots"}, "must be a number"},
		"empty list":       {domain.ToolRule{Input: "symbol", Kind: domain.RuleOneOf, Value: []any{}}, "between 1 and"},
		"not in the enum":  {domain.ToolRule{Input: "order_type", Kind: domain.RuleOneOf, Value: []any{"stop"}}, "must be one of limit, market"},
		"fixed wrong type": {domain.ToolRule{Input: "size", Kind: domain.RuleFixed, Value: "abc"}, "isn't a valid value"},
		"unknown kind":     {domain.ToolRule{Input: "size", Kind: "wat", Value: 1.0}, "unknown kind"},
		"message too long": {domain.ToolRule{Input: "size", Kind: domain.RuleMax, Value: 1.0, Message: strings.Repeat("x", 300)}, "too long"},
	} {
		tc.r.ToolID = "tool_trade"
		if _, err := domain.NormalizeRule(tc.r, ruleTradeTool()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error containing %q, got %v", name, tc.want, err)
		}
	}
	if _, err := domain.NormalizeRule(domain.ToolRule{ToolID: "x", Input: "size", Kind: domain.RuleMax, Value: 1.0}, nil); err == nil {
		t.Error("a rule for a tool that doesn't exist is refused")
	}
	n, err := domain.NormalizeRule(domain.ToolRule{Input: "size", Kind: domain.RuleMax, Value: "100000"}, ruleTradeTool())
	if err != nil || n.Value != 100000.0 {
		t.Errorf("a numeric string is made a number: %+v %v", n, err)
	}
}

func TestNormalizeRules_RefusesRulesThatCannotBothHold(t *testing.T) {
	tools := map[string]*domain.ToolDefinition{"tool_trade": ruleTradeTool()}
	r := func(in string, k domain.RuleKind, v any) domain.ToolRule {
		return domain.ToolRule{ToolID: "tool_trade", Input: in, Kind: k, Value: v}
	}
	for name, tc := range map[string]struct {
		rules []domain.ToolRule
		want  string
	}{
		"min above max":     {[]domain.ToolRule{r("size", domain.RuleMax, 10.0), r("size", domain.RuleMin, 20.0)}, "smallest allowed for size is bigger"},
		"two maximums":      {[]domain.ToolRule{r("size", domain.RuleMax, 10.0), r("size", domain.RuleMax, 20.0)}, "already has a largest"},
		"fixed and default": {[]domain.ToolRule{r("min_rr", domain.RuleFixed, 2.0), r("min_rr", domain.RuleDefault, 1.0)}, "can have only one"},
		"unknown tool":      {[]domain.ToolRule{{ToolID: "ghost", Input: "size", Kind: domain.RuleMax, Value: 1.0}}, "doesn't exist"},
	} {
		if _, err := domain.NormalizeRules(tc.rules, tools); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	out, err := domain.NormalizeRules([]domain.ToolRule{r("size", domain.RuleMin, 1.0), r("size", domain.RuleMax, 10.0)}, tools)
	if err != nil || len(out) != 2 {
		t.Errorf("a compatible set is accepted: %v %v", out, err)
	}
}

// The rules held on the agent reach its tools through the way skills are built, for tools bound
// one by one and for tools granted through a whole integration alike.
func TestHydrateSkillTools_AppliesTheAgentsRules(t *testing.T) {
	exec := &ruleExecutor{}
	reg := domain.NewExecutorRegistry()
	reg.Register("trade", exec)
	integration := &domain.Integration{ID: "int_1", Service: "trade"}
	tool := ruleTradeTool()
	agent := &domain.Agent{Rules: []domain.ToolRule{
		{ToolID: "tool_trade", Input: "size", Kind: domain.RuleMax, Value: 100.0},
	}}
	tools := map[string]*domain.ToolDefinition{tool.ID: tool}
	integrations := map[string]*domain.Integration{integration.ID: integration}

	for name, skill := range map[string]*domain.Skill{
		"bound one by one":       {ID: "s1", Tools: []domain.ToolBinding{{ToolID: tool.ID}}},
		"granted by integration": {ID: "s2", Integrations: []string{integration.ID}},
	} {
		bound, err := domain.HydrateSkillTools(skill, agent, tools, nil, integrations, reg)
		if err != nil || len(bound) != 1 {
			t.Fatalf("%s: %v %d", name, err, len(bound))
		}
		before := exec.n()
		out, _ := bound[0].Invoke(context.Background(), map[string]any{"symbol": "EURUSD", "size": 101.0, "min_rr": 2.0})
		if _, ok := refused(out); !ok || exec.n() != before {
			t.Errorf("%s: the agent's rule must apply: %v", name, out)
		}
		if _, err := bound[0].Invoke(context.Background(), map[string]any{"symbol": "EURUSD", "size": 100.0, "min_rr": 2.0}); err != nil || exec.n() != before+1 {
			t.Errorf("%s: within the rule it runs", name)
		}
	}

	// Another agent, with no rules, is not held to this one's.
	bound, _ := domain.HydrateSkillTools(&domain.Skill{ID: "s3", Tools: []domain.ToolBinding{{ToolID: tool.ID}}}, &domain.Agent{}, tools, nil, integrations, reg)
	before := exec.n()
	bound[0].Invoke(context.Background(), map[string]any{"symbol": "EURUSD", "size": 5000.0, "min_rr": 2.0})
	if exec.n() != before+1 {
		t.Error("rules belong to one agent only")
	}
}
