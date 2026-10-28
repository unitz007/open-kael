package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// fxExec stands in for the service behind the tools: quotes work; the trade returns
// whatever the test scripts (a real placement, a rejection…).
type fxExec struct {
	mu     sync.Mutex
	tradeN int
	trade  any
}

func (e *fxExec) Execute(_ context.Context, _ *domain.Identity, _ string, action string, _ map[string]any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if action == "t" {
		e.tradeN++
		return e.trade, nil
	}
	return "1.1253", nil
}

// scriptedModel plays the agent: step n decides what it does. It also keeps what the
// agent was offered, so tests can see the finish action it was given.
type scriptedModel struct {
	mu     sync.Mutex
	n      int
	step   func(n int) *domain.LLMResponse
	finish *domain.ActionSpec
	names  []string
	system string // the instructions it was last given
}

func (m *scriptedModel) Call(_ context.Context, msgs []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	if len(msgs) > 0 && msgs[0].Role == domain.RoleSystem {
		m.system = msgs[0].Content
	}
	if m.n == 1 {
		for i := range actions {
			if actions[i].Name == domain.FinishActionName {
				a := actions[i]
				m.finish = &a
			} else {
				m.names = append(m.names, actions[i].Name)
			}
		}
	}
	return m.step(m.n), nil
}

type fakeRequirer struct {
	names   []string
	err     error
	asked   int
	passed  []string
	earlier string
}

func (r *fakeRequirer) RequiredTools(_ context.Context, _ string, earlier string, tools []*domain.BoundAction) ([]string, error) {
	r.asked++
	r.earlier = earlier
	for _, t := range tools {
		r.passed = append(r.passed, t.Spec.Name)
	}
	return r.names, r.err
}

type keepOnly struct{ keep string }

func (k keepOnly) FilterTools(_ context.Context, _ string, tools []*domain.BoundAction) ([]*domain.BoundAction, error) {
	var out []*domain.BoundAction
	for _, t := range tools {
		if t.Spec.Name == k.keep {
			out = append(out, t)
		}
	}
	return out, nil
}

func finishCall(content, outcome string) domain.ToolCall {
	args := map[string]any{"content": content}
	if outcome != "" {
		args["outcome"] = outcome
	}
	return domain.ToolCall{ID: "f", Name: domain.FinishActionName, Arguments: args}
}

type rig struct {
	model   *scriptedModel
	exec    *fxExec
	host    *runtime.Host
	mem     *memMemory            // kept between turns, so a conversation has a history
	prereqs []domain.Prerequisite // what the capability says must run first
	user    string                // who is talking (none when empty)
}

func (r *rig) turn(t *testing.T, intent domain.Intent, text string) *domain.LoopResult {
	t.Helper()
	integration := &domain.Integration{ID: "int-fx", Service: "fx"}
	skill := &domain.Skill{ID: "s1", Name: "forex", AgentID: "a1", Integrations: []string{integration.ID}, Prerequisites: r.prereqs}
	agent := &domain.Agent{ID: "a1", LLMs: []domain.LLM{r.model}, MaxIterations: 8, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &fakeExecutor{})
	executors.Register("fx", r.exec)
	quote := &domain.ToolDefinition{ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Action: "q", ReadOnly: true, IntegrationID: integration.ID}
	trade := &domain.ToolDefinition{ID: "t", Name: "place_trade", FunctionName: "place_trade", Action: "t", IntegrationID: integration.ID}
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		if r.mem == nil {
			r.mem = newMemMemory()
		}
		d.Memory = r.mem
		d.ToolsByID = map[string]*domain.ToolDefinition{"q": quote, "t": trade}
		d.IntegrationsByID[integration.ID] = integration
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	r.host.SetSkillRouter(oneSkillRouter{skill: "forex", intent: intent})
	r.host.Register(hosted)
	res, err := r.host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1", UserID: r.user}, text)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	return res
}

func newRig(step func(n int) *domain.LLMResponse, trade any) *rig {
	return &rig{model: &scriptedModel{step: step}, exec: &fxExec{trade: trade}, host: runtime.NewHost()}
}

func calls(cs ...domain.ToolCall) *domain.LLMResponse { return &domain.LLMResponse{ToolCalls: cs} }

var placed = map[string]any{"status": "placed", "changed": true, "message": "BUY EURUSD order placed on your demo account (order 42)."}

// The message that started all this: the agent says the trade is placed and never
// calls the trade tool. It must never reach the person; the trade must really run.
func TestRequirements_AMadeUpTradeNeverReachesThePerson(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(domain.ToolCall{ID: "q", Name: "get_live_quote", Arguments: map[string]any{}}) // looks up a price…
		case 2:
			return calls(finishCall("✅ Trade Placed Successfully! EUR/USD BUY 50,000 units — Executed", "done")) // …then makes it up
		case 3:
			return calls(domain.ToolCall{ID: "t", Name: "place_trade", Arguments: map[string]any{}})
		default:
			return calls(finishCall("Your trade is in.", "done"))
		}
	}, placed)
	r.host.SetToolRequirer(&fakeRequirer{names: []string{"place_trade"}})
	res := r.turn(t, domain.IntentExecute, "buy 50000 EURUSD")
	if r.exec.tradeN != 1 {
		t.Fatalf("the trade must really have been run, ran %d time(s)", r.exec.tradeN)
	}
	if strings.Contains(res.Content, "Executed") || strings.Contains(res.Content, "50,000") {
		t.Fatalf("the made-up message must never be delivered: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Your trade is in.") || !strings.Contains(res.Content, "✓ BUY EURUSD order placed on your demo account (order 42).") {
		t.Errorf("the person should get the agent's words and the real receipt: %q", res.Content)
	}
}

// The agent is told, in the finish action itself, how it must end.
func TestRequirements_TheAgentIsAskedHowItEnded(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("I need your size.", "need_details")) }, placed)
	r.host.SetToolRequirer(&fakeRequirer{names: []string{"place_trade"}})
	res := r.turn(t, domain.IntentExecute, "buy EURUSD")
	if r.model.finish == nil {
		t.Fatal("no finish action offered")
	}
	out, ok := r.model.finish.InputSchema.Properties["outcome"]
	if !ok || len(out.Enum) != 3 {
		t.Fatalf("finish must ask for an outcome of done / need_details / cannot: %+v", r.model.finish.InputSchema)
	}
	if res.Content != "I need your size." || r.exec.tradeN != 0 {
		t.Errorf("an honest 'need details' ending goes straight through: %q", res.Content)
	}
}

func TestRequirements_ARequestThatNeedsNoActionIsNotHeldToAnything(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("Just chatting.", "")) }, placed)
	req := &fakeRequirer{names: nil}
	r.host.SetToolRequirer(req)
	res := r.turn(t, domain.IntentExecute, "how are you")
	if _, has := r.model.finish.InputSchema.Properties["outcome"]; has || res.Content != "Just chatting." {
		t.Errorf("nothing required, nothing asked: %+v %q", r.model.finish.InputSchema.Properties, res.Content)
	}
}

func TestRequirements_AdviceIsNeverHeldToAnAction_AndJevIsNotAsked(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("I'd wait.", "")) }, placed)
	req := &fakeRequirer{names: []string{"place_trade"}}
	r.host.SetToolRequirer(req)
	res := r.turn(t, domain.IntentRecommend, "what do you suggest")
	if req.asked != 0 || res.Content != "I'd wait." {
		t.Errorf("advice must not be held to an action: asked=%d %q", req.asked, res.Content)
	}
}

func TestRequirements_JevIsOnlyAskedAboutToolsThatChangeThings(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("ok", "need_details")) }, placed)
	req := &fakeRequirer{names: []string{"place_trade"}}
	r.host.SetToolRequirer(req)
	r.turn(t, domain.IntentExecute, "buy EURUSD")
	if strings.Join(req.passed, ",") != "place_trade" {
		t.Errorf("only the tool that changes something is put to Jev: %v", req.passed)
	}
}

// If Jev can't say, an action must still really have happened.
func TestRequirements_WhenJevCannotSayAnyRealChangeIsRequired(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		if n <= 3 {
			return calls(finishCall("Done!", "done")) // never calls anything
		}
		return calls(finishCall("Done!", "done"))
	}, placed)
	r.host.SetToolRequirer(&fakeRequirer{err: errors.New("jev down")})
	res := r.turn(t, domain.IntentExecute, "buy EURUSD")
	if !strings.HasPrefix(res.Content, "Nothing was changed:") {
		t.Errorf("a claim with no change behind it must not go through even when Jev is down: %q", res.Content)
	}
}

// The relevance filter must not be able to take away the very tool the request needs.
func TestRequirements_TheFilterCannotRemoveTheRequiredTool(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("ok", "need_details")) }, placed)
	r.host.SetToolRequirer(&fakeRequirer{names: []string{"place_trade"}})
	r.host.SetToolFilter(keepOnly{keep: "get_live_quote"}) // would drop place_trade
	r.turn(t, domain.IntentExecute, "buy EURUSD")
	have := map[string]bool{}
	for _, n := range r.model.names {
		have[n] = true
	}
	if !have["place_trade"] || !have["get_live_quote"] {
		t.Errorf("the required tool must stay on offer: %v", r.model.names)
	}
}

func TestRequirements_ARejectedTradeCannotBeCalledDone(t *testing.T) {
	rejected := map[string]any{"status": "rejected", "changed": false, "message": "The trade was rejected: RR 1.2 is below the minimum you set."}
	r := newRig(func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(domain.ToolCall{ID: "t", Name: "place_trade", Arguments: map[string]any{}})
		default:
			return calls(finishCall("Your trade is live!", "done"))
		}
	}, rejected)
	r.host.SetToolRequirer(&fakeRequirer{names: []string{"place_trade"}})
	res := r.turn(t, domain.IntentExecute, "buy EURUSD")
	if strings.Contains(res.Content, "live") || !strings.Contains(res.Content, "Nothing was changed: The trade was rejected: RR 1.2 is below the minimum you set") {
		t.Errorf("the person gets the real reason, not the claim: %q", res.Content)
	}
}

// Without a requirer the host behaves exactly as before.
func TestRequirements_NoRequirerNoChange(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("Trade placed!", "")) }, placed)
	res := r.turn(t, domain.IntentExecute, "buy EURUSD")
	if _, has := r.model.finish.InputSchema.Properties["outcome"]; has || res.Content != "Trade placed!" {
		t.Errorf("feature off means untouched: %q", res.Content)
	}
}

// "yes" only means something next to the question it answers: Jev is shown the
// assistant's previous message, not just the one word.
func TestRequirements_ShortFollowUpsAreJudgedNextToWhatTheyAnswer(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		if n == 1 {
			return calls(finishCall("I'd BUY EUR/USD, 50,000 units. Shall I place it?", "need_details"))
		}
		return calls(finishCall("ok", "need_details"))
	}, placed)
	req := &fakeRequirer{names: []string{"place_trade"}}
	r.host.SetToolRequirer(req)
	r.turn(t, domain.IntentExecute, "buy EURUSD")
	if req.earlier != "" {
		t.Errorf("nothing was said before the first message: %q", req.earlier)
	}
	r.turn(t, domain.IntentExecute, "yes")
	if !strings.Contains(req.earlier, "Shall I place it?") {
		t.Errorf("Jev must be shown what the person is answering: %q", req.earlier)
	}
}

// ── the capability picker sees what a short reply answers ────────────────────

// ctxRouter is a router that can read a message next to the previous one, and
// remembers how it was asked.
type ctxRouter struct {
	mu         sync.Mutex
	plainCalls int
	ctxCalls   int
	earlier    []domain.Earlier
}

func (r *ctxRouter) PickSkill(context.Context, string, []*domain.Skill) (string, float64, domain.Intent, error) {
	return "forex", 1, domain.IntentExecute, nil
}
func (r *ctxRouter) PickSkills(context.Context, string, []*domain.Skill) ([]string, domain.Intent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plainCalls++
	return []string{"forex"}, domain.IntentExecute, nil
}
func (r *ctxRouter) PickSkillsInContext(_ context.Context, _ string, earlier domain.Earlier, _ []*domain.Skill) ([]string, domain.Intent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctxCalls++
	r.earlier = append(r.earlier, earlier)
	return []string{"forex"}, domain.IntentExecute, nil
}

func (r *rig) turnWith(t *testing.T, router runtime.SkillRouter, text string) *domain.LoopResult {
	t.Helper()
	integration := &domain.Integration{ID: "int-fx", Service: "fx"}
	skill := &domain.Skill{ID: "s1", Name: "forex", AgentID: "a1", Integrations: []string{integration.ID}}
	agent := &domain.Agent{ID: "a1", LLMs: []domain.LLM{r.model}, MaxIterations: 8, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &fakeExecutor{})
	executors.Register("fx", r.exec)
	quote := &domain.ToolDefinition{ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Action: "q", ReadOnly: true, IntegrationID: integration.ID}
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		if r.mem == nil {
			r.mem = newMemMemory()
		}
		d.Memory = r.mem
		d.ToolsByID = map[string]*domain.ToolDefinition{"q": quote}
		d.IntegrationsByID[integration.ID] = integration
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	r.host.SetSkillRouter(router)
	r.host.Register(hosted)
	res, err := r.host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1", UserID: r.user}, text)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	return res
}

func TestRouter_AShortReplyIsReadNextToWhatItAnswers(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		return calls(finishCall("I'd BUY EUR/USD, 50,000 units. Shall I place it?", ""))
	}, placed)
	router := &ctxRouter{}
	r.turnWith(t, router, "buy EURUSD") // first message: nothing earlier
	if router.plainCalls != 1 || router.ctxCalls != 0 {
		t.Fatalf("with nothing earlier the ordinary picker is used: plain=%d ctx=%d", router.plainCalls, router.ctxCalls)
	}
	r.turnWith(t, router, "yes") // answers the offer
	if router.ctxCalls != 1 || router.plainCalls != 1 {
		t.Fatalf("a reply is read in context: plain=%d ctx=%d", router.plainCalls, router.ctxCalls)
	}
	if len(router.earlier) != 1 || !strings.Contains(router.earlier[0].Assistant, "Shall I place it?") || router.earlier[0].User != "buy EURUSD" {
		t.Errorf("the picker must be shown the offer the person is answering and what they had asked: %+v", router.earlier)
	}
}

// A router that can't read context keeps working exactly as before.
func TestRouter_APickerWithoutContextSupportIsStillUsed(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("Shall I place it?", "")) }, placed)
	router := oneSkillRouter{skill: "forex", intent: domain.IntentExecute}
	r.turnWith(t, router, "buy EURUSD")
	res := r.turnWith(t, router, "yes")
	if res.Content != "Shall I place it?" {
		t.Errorf("a plain router is unaffected: %q", res.Content)
	}
}

// A request to do something keeps every tool that does things, whatever the relevance filter thinks: only look-ups are
// trimmed. ("…but let's confirm the size first" once lost place_trade, so the trade could not be placed.)
func TestFilter_ARequestToDoSomethingKeepsEveryToolThatDoesThings(t *testing.T) {
	r := newRig(func(int) *domain.LLMResponse { return calls(finishCall("ok", "need_details")) }, placed)
	r.host.SetToolFilter(keepOnly{keep: "get_live_quote"}) // would drop place_trade
	r.turn(t, domain.IntentExecute, "I want in on the GBPUSD sell, but let's confirm the size first")
	have := map[string]bool{}
	for _, n := range r.model.names {
		have[n] = true
	}
	if !have["place_trade"] || !have["get_live_quote"] {
		t.Errorf("place_trade stays on offer for a request to do something: %v", r.model.names)
	}
}

// "Check before placing a trade", kept by the platform in a real turn: the agent tries to place it first, is told to
// check, checks, then places it. And the relevance filter, which would offer only the trade, can't take the check away.
func TestPrerequisite_InATurnTheTradeWaitsForTheCheck(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(domain.ToolCall{ID: "1", Name: "place_trade", Arguments: map[string]any{}})
		case 2:
			return calls(domain.ToolCall{ID: "2", Name: "get_live_quote", Arguments: map[string]any{}})
		case 3:
			return calls(domain.ToolCall{ID: "3", Name: "place_trade", Arguments: map[string]any{}})
		}
		return calls(finishCall("Placed.", "done"))
	}, placed)
	r.prereqs = []domain.Prerequisite{{Before: "t", First: "q"}}
	r.host.SetToolRequirer(&fakeRequirer{names: []string{"place_trade"}})
	r.host.SetToolFilter(keepOnly{keep: "place_trade"}) // would drop the check
	res := r.turn(t, domain.IntentExecute, "buy EURUSD now, don't bother checking anything")
	have := map[string]bool{}
	for _, n := range r.model.names {
		have[n] = true
	}
	if !have["get_live_quote"] {
		t.Fatalf("the check the trade needs stays on offer: %v", r.model.names)
	}
	if r.exec.tradeN != 1 || r.model.n != 4 {
		t.Fatalf("one trade, after the check: trades=%d model calls=%d", r.exec.tradeN, r.model.n)
	}
	if !strings.Contains(res.Content, "Placed") {
		t.Errorf("%q", res.Content)
	}
}
