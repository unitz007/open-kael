package domain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// tradingExecutor records what really reached the service.
type tradingExecutor struct {
	mu   sync.Mutex
	seen []string
	fail bool
}

func (e *tradingExecutor) Execute(_ context.Context, _ *domain.Identity, _ string, action string, input map[string]any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, action+":"+strings.ToUpper(strings.TrimSpace(input["symbol"].(string))))
	if e.fail && action == "status" {
		return map[string]any{"status": "error", "message": "broker down"}, nil
	}
	return map[string]any{"ok": true}, nil
}

func tradingFixture(exec *tradingExecutor) (map[string]*domain.ToolDefinition, map[string]*domain.Integration, *domain.ExecutorRegistry) {
	symbol := domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString, Description: "The pair."}}, Required: []string{"symbol"}}
	tools := map[string]*domain.ToolDefinition{
		"t:status": {ID: "t:status", Name: "get_trading_status", Description: "Open trades.", IntegrationID: "trading", Action: "status", ReadOnly: true, InputSchema: symbol},
		"t:place":  {ID: "t:place", Name: "place_trade", Description: "Places a trade.", IntegrationID: "trading", Action: "place", InputSchema: symbol},
		"t:news":   {ID: "t:news", Name: "news", Description: "News.", IntegrationID: "trading", Action: "news", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}},
	}
	reg := domain.NewExecutorRegistry()
	reg.Register("trading", exec)
	return tools, map[string]*domain.Integration{"trading": {ID: "trading", Service: "trading"}}, reg
}

func action(t *testing.T, bound []*domain.BoundAction, name string) *domain.BoundAction {
	t.Helper()
	for _, a := range bound {
		if a.Spec.Name == name {
			return a
		}
	}
	t.Fatalf("no %s", name)
	return nil
}

// "Check open trades on the pair before placing a trade", kept by the platform: the trade is refused, back to the
// model, until the check has run successfully in the same run for the same pair; nothing reaches the service before.
func TestPrerequisite_TheTradeWaitsForTheCheckOnTheSamePair(t *testing.T) {
	exec := &tradingExecutor{}
	tools, ints, reg := tradingFixture(exec)
	skill := &domain.Skill{ID: "s", Integrations: []string{"trading"},
		Prerequisites: []domain.Prerequisite{{Before: "t:place", First: "t:status", Same: []string{"symbol"}}}}
	bound, err := domain.HydrateSkillTools(skill, &domain.Agent{}, tools, nil, ints, reg)
	if err != nil {
		t.Fatal(err)
	}
	place, status := action(t, bound, "place_trade"), action(t, bound, "get_trading_status")
	if len(place.Needs) != 1 || place.Needs[0] != "get_trading_status" || len(status.Needs) != 0 {
		t.Errorf("what it needs is said, for whoever trims what is offered: %v", place.Needs)
	}
	ctx := domain.WithRunJournal(context.Background())

	out, _ := place.Invoke(ctx, map[string]any{"symbol": "EURUSD"})
	if m, _ := out.(map[string]any); m["status"] != "refused" || !strings.Contains(m["message"].(string), "run get_trading_status first for the same symbol EURUSD") {
		t.Fatalf("refused, saying what to run first: %v", out)
	}
	if len(exec.seen) != 0 {
		t.Fatalf("nothing reached the service: %v", exec.seen)
	}
	if _, err := status.Invoke(ctx, map[string]any{"symbol": "GBPUSD"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := place.Invoke(ctx, map[string]any{"symbol": "EURUSD"}); out.(map[string]any)["status"] != "refused" {
		t.Fatal("a check on another pair is not this one")
	}
	if _, err := status.Invoke(ctx, map[string]any{"symbol": "eurusd "}); err != nil {
		t.Fatal(err)
	}
	if out, _ := place.Invoke(ctx, map[string]any{"symbol": "EURUSD"}); out.(map[string]any)["ok"] != true {
		t.Fatalf("after the check on the same pair it goes through: %v", out)
	}
	if strings.Join(exec.seen, ",") != "status:GBPUSD,status:EURUSD,place:EURUSD" {
		t.Errorf("%v", exec.seen)
	}
	// Another run starts again: what was checked before doesn't count.
	if out, _ := place.Invoke(domain.WithRunJournal(context.Background()), map[string]any{"symbol": "EURUSD"}); out.(map[string]any)["status"] != "refused" {
		t.Error("a new run needs its own check")
	}
}

// Only a check that really went through counts: one the service refused is not written down.
func TestPrerequisite_AFailedCheckDoesNotCount(t *testing.T) {
	exec := &tradingExecutor{fail: true}
	tools, ints, reg := tradingFixture(exec)
	skill := &domain.Skill{ID: "s", Integrations: []string{"trading"},
		Prerequisites: []domain.Prerequisite{{Before: "t:place", First: "t:status", Same: []string{"symbol"}}}}
	bound, _ := domain.HydrateSkillTools(skill, &domain.Agent{}, tools, nil, ints, reg)
	ctx := domain.WithRunJournal(context.Background())
	_, _ = action(t, bound, "get_trading_status").Invoke(ctx, map[string]any{"symbol": "EURUSD"})
	if out, _ := action(t, bound, "place_trade").Invoke(ctx, map[string]any{"symbol": "EURUSD"}); out.(map[string]any)["status"] != "refused" {
		t.Fatalf("a failed check doesn't open the way: %v", out)
	}
}

// What can be saved: both abilities granted, the first only looking things up, the "same" inputs on both; repeats go.
func TestNormalizePrerequisites(t *testing.T) {
	tools, _, _ := tradingFixture(&tradingExecutor{})
	granted := map[string]*domain.ToolDefinition{"t:status": tools["t:status"], "t:place": tools["t:place"], "t:news": tools["t:news"]}
	ok := domain.Prerequisite{Before: "t:place", First: "t:status", Same: []string{"symbol", " "}, Text: " checks first "}
	got, err := domain.NormalizePrerequisites([]domain.Prerequisite{ok, ok}, granted)
	if err != nil || len(got) != 1 || len(got[0].Same) != 1 || got[0].Text != "checks first" {
		t.Fatalf("%+v %v", got, err)
	}
	for name, p := range map[string]domain.Prerequisite{
		"not granted":           {Before: "t:place", First: "t:other"},
		"itself":                {Before: "t:place", First: "t:place"},
		"first changes things":  {Before: "t:status", First: "t:place"},
		"same on only one side": {Before: "t:place", First: "t:news", Same: []string{"symbol"}},
	} {
		if _, err := domain.NormalizePrerequisites([]domain.Prerequisite{p}, granted); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	many := make([]domain.Prerequisite, domain.MaxPrerequisites+1)
	if _, err := domain.NormalizePrerequisites(many, granted); err == nil {
		t.Error("too many accepted")
	}
}
