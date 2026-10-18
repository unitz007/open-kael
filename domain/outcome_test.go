package domain_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestOutcomeOf(t *testing.T) {
	cases := []struct {
		name      string
		out       any
		err       error
		done      bool
		message   string
		reasonHas string
	}{
		{"plain success", "ok", nil, true, "", ""},
		{"an object with a message", map[string]any{"status": "placed", "message": "BUY placed", "changed": true}, nil, true, "BUY placed", ""},
		{"an object that doesn't say", map[string]any{"message": "sent"}, nil, true, "sent", ""},
		{"an error", nil, errors.New("boom: secret detail"), false, "", "didn't go through"},
		{"declined", domain.OutputNotApproved, nil, false, "", "wasn't approved"},
		{"sign-in pending", domain.OutputAuthPendingPrefix + ": the connect prompt has been sent", nil, false, "", "connect your account"},
		{"tool says it changed nothing", map[string]any{"changed": false, "message": "The trade was rejected: RR 1.2 is below the minimum you set."}, nil, false, "The trade was rejected: RR 1.2 is below the minimum you set.", "rejected"},
		{"changed nothing and said nothing", map[string]any{"changed": false}, nil, false, "", "nothing was changed"},
	}
	for _, c := range cases {
		done, msg, why := domain.OutcomeOf(c.out, c.err)
		if done != c.done || msg != c.message || (c.reasonHas != "" && !strings.Contains(why, c.reasonHas)) {
			t.Errorf("%s: got done=%v msg=%q reason=%q", c.name, done, msg, why)
		}
		if strings.Contains(why, "secret detail") {
			t.Errorf("%s: the reason must not leak the raw error: %q", c.name, why)
		}
	}
}

// ── the loop ──────────────────────────────────────────────────────────────────

func finishWithOutcome() *domain.BoundAction {
	return &domain.BoundAction{Spec: domain.ActionSpec{
		Name: domain.FinishActionName,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{
				"content": {Type: domain.SchemaTypeString},
				"outcome": {Type: domain.SchemaTypeString, Enum: []string{"done", "need_details", "cannot"}},
			},
			Required: []string{"content", "outcome"}},
	}}
}

func plainFinish() *domain.BoundAction {
	return &domain.BoundAction{Spec: domain.ActionSpec{
		Name: domain.FinishActionName,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"content": {Type: domain.SchemaTypeString}}}}}
}

// tradeTool is a tool that changes something; what it returns is scripted.
func tradeTool(name string, calls *int, out any, err error) *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{Name: name, Changes: true},
		Invoke: func(context.Context, map[string]any) (any, error) {
			*calls++
			return out, err
		},
	}
}

func lookup(name string) *domain.BoundAction {
	return &domain.BoundAction{
		Spec:   domain.ActionSpec{Name: name, ReadOnly: true},
		Invoke: func(context.Context, map[string]any) (any, error) { return "data", nil },
	}
}

func call(id, name string, args map[string]any) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: name, Arguments: args}
}

func say(calls ...domain.ToolCall) *domain.LLMResponse { return &domain.LLMResponse{ToolCalls: calls} }

func finishCall(id, content, outcome string) domain.ToolCall {
	args := map[string]any{"content": content}
	if outcome != "" {
		args["outcome"] = outcome
	}
	return call(id, domain.FinishActionName, args)
}

func runLoop(t *testing.T, ctx context.Context, script []*domain.LLMResponse, actions ...*domain.BoundAction) (*domain.LoopResult, int) {
	t.Helper()
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n > len(script) {
			t.Fatalf("the loop asked the model %d times; the script has %d steps", n, len(script))
		}
		return script[n-1], nil
	}}
	res, _, err := domain.NewNativeLoop([]domain.LLM{llm}, 10).Run(ctx, []domain.Message{{Role: domain.RoleUser, Content: "place a trade"}}, actions)
	if err != nil {
		t.Fatal(err)
	}
	return res, llm.n
}

func needTrade() context.Context {
	return domain.WithRequirements(context.Background(), &domain.Requirements{Tools: []string{"place_trade"}})
}

// THE case: the agent says the trade is placed and never called the trade tool.
func TestLoop_AFalseDoneIsRefusedAndTheAgentIsSentToDoItForReal(t *testing.T) {
	calls := 0
	trade := tradeTool("place_trade", &calls, map[string]any{"status": "placed", "changed": true, "message": "BUY EURUSD order placed on your demo account (order 42)."}, nil)
	res, asked := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(finishCall("1", "Trade Placed Successfully! 🎉 EUR/USD BUY 50,000 units — Executed", "done")), // the made-up message
		say(call("2", "place_trade", nil)), // told it hasn't, it now does it for real
		say(finishCall("3", "Done — your trade is in.", "done")),
	}, trade, finishWithOutcome())
	if calls != 1 {
		t.Fatalf("the trade tool must have been run for real, ran %d time(s)", calls)
	}
	if asked != 3 || strings.Contains(res.Content, "Executed") {
		t.Errorf("the made-up text must never be delivered: asked=%d content=%q", asked, res.Content)
	}
	if !strings.Contains(res.Content, "Done — your trade is in.") || !strings.Contains(res.Content, "✓ BUY EURUSD order placed on your demo account (order 42).") {
		t.Errorf("the real finish should carry the tool's own receipt: %q", res.Content)
	}
}

func TestLoop_AgentThatKeepsClaimingGetsTheFactsInstead(t *testing.T) {
	calls := 0
	trade := tradeTool("place_trade", &calls, "should never run", nil)
	res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(finishCall("1", "Trade placed!", "done")),
		say(finishCall("2", "Trade placed!", "done")),
		say(finishCall("3", "Trade placed!", "done")),
	}, trade, finishWithOutcome())
	if strings.Contains(res.Content, "Trade placed") || !strings.HasPrefix(res.Content, "Nothing was changed:") {
		t.Errorf("after the refusals the person gets the facts, not the claim: %q", res.Content)
	}
	if calls != 0 {
		t.Errorf("the loop must not run the tool itself")
	}
}

func TestLoop_TheTwoHonestEndingsAreAlwaysAccepted(t *testing.T) {
	for _, outcome := range []string{"need_details", "cannot"} {
		calls := 0
		res, asked := runLoop(t, needTrade(), []*domain.LLMResponse{
			say(finishCall("1", "I need your size first.", outcome)),
		}, tradeTool("place_trade", &calls, "x", nil), finishWithOutcome())
		if asked != 1 || res.Content != "I need your size first." || calls != 0 {
			t.Errorf("%s: should be accepted untouched, asked=%d content=%q", outcome, asked, res.Content)
		}
	}
}

func TestLoop_ATradeTheBrokerRejectedCannotBeCalledDone(t *testing.T) {
	calls := 0
	trade := tradeTool("place_trade", &calls, map[string]any{"status": "rejected", "changed": false, "message": "The trade was rejected: RR 1.2 is below the minimum you set."}, nil)
	res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(call("1", "place_trade", nil)),
		say(finishCall("2", "Your trade is live!", "done")),                                 // refused, with the reason
		say(finishCall("3", "It was rejected: the ratio is below your minimum.", "cannot")), // honest
	}, trade, finishWithOutcome())
	if res.Content != "It was rejected: the ratio is below your minimum." {
		t.Errorf("the honest ending should come through: %q", res.Content)
	}
	if len(res.Actions) != 1 || res.Actions[0].Done || !strings.Contains(res.Actions[0].Reason, "rejected") {
		t.Errorf("the record should say it did not go through, and why: %+v", res.Actions)
	}
}

func TestLoop_DeclinedAndSignInPendingAreNotDone(t *testing.T) {
	for name, out := range map[string]any{"declined": domain.OutputNotApproved, "sign-in pending": domain.OutputAuthPendingPrefix + ": prompt sent"} {
		calls := 0
		res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
			say(call("1", "place_trade", nil)),
			say(finishCall("2", "Done!", "done")),
			say(finishCall("3", "Done!", "done")),
			say(finishCall("4", "Done!", "done")),
		}, tradeTool("place_trade", &calls, out, nil), finishWithOutcome())
		if !strings.HasPrefix(res.Content, "Nothing was changed:") {
			t.Errorf("%s: %q", name, res.Content)
		}
	}
}

func TestLoop_AnErroringToolIsNotDone_AndTheRawErrorStaysOutOfTheReply(t *testing.T) {
	calls := 0
	res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(call("1", "place_trade", nil)),
		say(finishCall("2", "Done!", "done")),
		say(finishCall("3", "Done!", "done")),
		say(finishCall("4", "Done!", "done")),
	}, tradeTool("place_trade", &calls, nil, errors.New("dial tcp 10.0.0.1: secret internal detail")), finishWithOutcome())
	if !strings.Contains(res.Content, "didn't go through") || strings.Contains(res.Content, "10.0.0.1") {
		t.Errorf("plain reason, no internals: %q", res.Content)
	}
}

func TestLoop_ANeededOutcomeIsAskedForIfMissing(t *testing.T) {
	calls := 0
	res, asked := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(finishCall("1", "Hello", "")), // no outcome
		say(finishCall("2", "I can't without your size.", "need_details")),
	}, tradeTool("place_trade", &calls, "x", nil), finishWithOutcome())
	if asked != 2 || res.Content != "I can't without your size." {
		t.Errorf("a finish with no outcome is sent back: asked=%d %q", asked, res.Content)
	}
}

func TestLoop_TheWrongToolSucceedingDoesNotSatisfyTheRequest(t *testing.T) {
	n1, n2 := 0, 0
	res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(call("1", "close_trade", nil)), // a different changing tool, which works
		say(finishCall("2", "Trade placed!", "done")),
		say(finishCall("3", "Trade placed!", "done")),
		say(finishCall("4", "Trade placed!", "done")),
	}, tradeTool("close_trade", &n1, "closed", nil), tradeTool("place_trade", &n2, "x", nil), finishWithOutcome())
	// The request is not satisfied (only the tool it needs counts), but something DID go
	// through, so the reply must never say "nothing was changed".
	if !strings.HasPrefix(res.Content, "Part of that went through") || strings.Contains(res.Content, "Nothing was changed") || n2 != 0 {
		t.Errorf("only the tool this request needs counts, and what went through is not denied: %q", res.Content)
	}
}

// With no named tool (Jev couldn't say), any one successful change satisfies it.
func TestLoop_AnyChangeFallback(t *testing.T) {
	ctx := domain.WithRequirements(context.Background(), &domain.Requirements{AnyChange: true})
	a := 0
	res, _ := runLoop(t, ctx, []*domain.LLMResponse{
		say(call("1", "lookup_price", nil)), // a look-up never counts
		say(finishCall("2", "Done", "done")),
		say(call("3", "send_it", nil)),
		say(finishCall("4", "Sent.", "done")),
	}, lookup("lookup_price"), tradeTool("send_it", &a, map[string]any{"message": "Message sent."}, nil), finishWithOutcome())
	if res.Content != "Sent.\n\n✓ Message sent." {
		t.Errorf("a look-up must not satisfy it; a real change must: %q", res.Content)
	}
}

// Nothing changes for a turn with no requirements, or a finish that asks for no outcome.
func TestLoop_NoRequirementsMeansNoChange(t *testing.T) {
	res, asked := runLoop(t, context.Background(), []*domain.LLMResponse{
		say(finishCall("1", "Hi there", "done")),
	}, finishWithOutcome())
	if asked != 1 || res.Content != "Hi there" {
		t.Errorf("no requirements, no checks: %q", res.Content)
	}
}

func TestLoop_ANestedLoopWhoseFinishHasNoOutcomeIsNotHeldToTheRequirements(t *testing.T) {
	res, asked := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(finishCall("1", "inner result", "")),
	}, plainFinish())
	if asked != 1 || res.Content != "inner result" {
		t.Errorf("only a finish that asks for an outcome is checked: asked=%d %q", asked, res.Content)
	}
}

// Every call in a response needs an answer, even when finish is refused first.
func TestLoop_ARefusedFinishStillAnswersTheOtherCallsInThatResponse(t *testing.T) {
	calls := 0
	llm := &funcLLM{}
	llm.call = func(n int) (*domain.LLMResponse, error) {
		switch n {
		case 1:
			return say(finishCall("f", "Done!", "done"), call("x", "lookup_price", nil)), nil
		default:
			return say(finishCall("g", "I can't.", "cannot")), nil
		}
	}
	rec := &recordingLLM{inner: llm}
	res, msgs, err := domain.NewNativeLoop([]domain.LLM{rec}, 10).Run(needTrade(), []domain.Message{{Role: domain.RoleUser, Content: "go"}},
		[]*domain.BoundAction{tradeTool("place_trade", &calls, "x", nil), lookup("lookup_price"), finishWithOutcome()})
	if err != nil || res.Content != "I can't." {
		t.Fatalf("%v %q", err, res.Content)
	}
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == domain.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	if !answered["f"] || !answered["x"] {
		t.Errorf("both calls of the refused response need a tool message: %v", answered)
	}
}

type recordingLLM struct{ inner *funcLLM }

func (r *recordingLLM) Call(ctx context.Context, m []domain.Message, a []domain.ActionSpec) (*domain.LLMResponse, error) {
	return r.inner.Call(ctx, m, a)
}

// THE incident: "add a weekly team meeting" fits both create_event and create_recurring_event,
// so both were required. The agent correctly used one; the guard refused its "done" until it
// repeated the action (a duplicate calendar event) and then said "Nothing was changed".
// Required actions are alternatives: any one succeeding is enough.
func TestLoop_AnyOneOfTheFittingActionsIsEnough(t *testing.T) {
	ctx := domain.WithRequirements(context.Background(), &domain.Requirements{Tools: []string{"create_event", "create_recurring_event"}})
	single, recurring := 0, 0
	res, asked := runLoop(t, ctx, []*domain.LLMResponse{
		say(call("1", "create_recurring_event", map[string]any{"summary": "Team meeting"})),
		say(finishCall("2", "Added your weekly team meeting.", "done")),
	}, tradeTool("create_event", &single, map[string]any{"message": "event added"}, nil),
		tradeTool("create_recurring_event", &recurring, map[string]any{"message": "weekly event added"}, nil), finishWithOutcome())
	if asked != 2 || recurring != 1 || single != 0 {
		t.Fatalf("one fitting action doing it is enough, and nothing is repeated: asked=%d recurring=%d single=%d", asked, recurring, single)
	}
	if !strings.Contains(res.Content, "Added your weekly team meeting.") || strings.Contains(res.Content, "Nothing was changed") {
		t.Errorf("the agent's own words stand, with the receipt: %q", res.Content)
	}
	if !strings.Contains(res.Content, "✓ weekly event added") {
		t.Errorf("receipt: %q", res.Content)
	}
}

// If the guard does have to give up, it says what really went through.
func TestLoop_GivingUpNeverDeniesWhatWentThrough(t *testing.T) {
	n, m := 0, 0
	res, _ := runLoop(t, needTrade(), []*domain.LLMResponse{
		say(call("1", "close_trade", nil)),
		say(finishCall("2", "Done!", "done")), say(finishCall("3", "Done!", "done")), say(finishCall("4", "Done!", "done")),
	}, tradeTool("close_trade", &n, map[string]any{"message": "Closed your EURUSD position."}, nil), tradeTool("place_trade", &m, "x", nil), finishWithOutcome())
	want := "This went through, but I couldn't confirm it was everything you asked for:\n✓ Closed your EURUSD position."
	if res.Content != want {
		t.Errorf("got %q\nwant %q", res.Content, want)
	}
}

func TestLoop_AnIdenticalChangeIsNeverMadeTwiceInOneTurn(t *testing.T) {
	n := 0
	args := map[string]any{"summary": "Team meeting", "start": "2026-10-19T10:00:00"}
	res, _ := runLoop(t, context.Background(), []*domain.LLMResponse{
		say(call("1", "create_event", args)),
		say(call("2", "create_event", map[string]any{"start": "2026-10-19T10:00:00", "summary": "Team meeting"})), // same change, keys in another order
		say(finishCall("3", "Added it.", "")),
	}, tradeTool("create_event", &n, map[string]any{"message": "added"}, nil), plainFinish())
	if n != 1 {
		t.Errorf("the same change must not be made twice: ran %d times", n)
	}
	_ = res

	// A genuinely different change is a different change.
	n = 0
	runLoop(t, context.Background(), []*domain.LLMResponse{
		say(call("1", "create_event", args)),
		say(call("2", "create_event", map[string]any{"summary": "Team meeting", "start": "2026-10-26T10:00:00"})),
		say(finishCall("3", "Added both.", "")),
	}, tradeTool("create_event", &n, map[string]any{"message": "added"}, nil), plainFinish())
	if n != 2 {
		t.Errorf("different arguments are a different change: ran %d times", n)
	}

	// A failed attempt may be retried: only what went through is remembered.
	n = 0
	calls := 0
	flaky := &domain.BoundAction{Spec: domain.ActionSpec{Name: "create_event", Changes: true}, Invoke: func(context.Context, map[string]any) (any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("temporary")
		}
		return map[string]any{"message": "added"}, nil
	}}
	runLoop(t, context.Background(), []*domain.LLMResponse{
		say(call("1", "create_event", args)), say(call("2", "create_event", args)), say(finishCall("3", "Added it.", "")),
	}, flaky, plainFinish())
	if calls != 2 {
		t.Errorf("a failed attempt can be retried: %d calls", calls)
	}
}
