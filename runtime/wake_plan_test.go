package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// scriptedLLM answers each model call with the next scripted step. A step may be built from what
// the previous tool calls returned, so a test can say "use what the look-up gave".
type scriptedLLM struct {
	mu    sync.Mutex
	steps []func(seen []domain.Message) []domain.ToolCall
	n     int
	saw   []domain.Message // the last conversation it was shown
}

func (s *scriptedLLM) Call(_ context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw = msgs
	if s.n >= len(s.steps) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "fin", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
	}
	step := s.steps[s.n]
	s.n++
	return &domain.LLMResponse{ToolCalls: step(msgs)}, nil
}

func call(id, name string, args map[string]any) []domain.ToolCall {
	return []domain.ToolCall{{ID: id, Name: name, Arguments: args}}
}

// wakeRig is an agent with one look-up (when does the next deadline happen) that the agent plans from.
type wakeRig struct {
	host   *runtime.Host
	llm    *scriptedLLM
	lookup *lookupCount
	now    time.Time
}

type lookupCount struct {
	mu sync.Mutex
	n  int
}

func (l *lookupCount) Execute(_ context.Context, _ *domain.Identity, _ string, action string, _ map[string]any) (any, error) {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	return map[string]any{"deadline": "2026-10-25T09:30:00Z"}, nil
}
func (l *lookupCount) count() int { l.mu.Lock(); defer l.mu.Unlock(); return l.n }

func newWakeRig(t *testing.T, readOnly bool, steps ...func([]domain.Message) []domain.ToolCall) *wakeRig {
	t.Helper()
	llm := &scriptedLLM{steps: steps}
	lk := &lookupCount{}
	integ := &domain.Integration{ID: "int-x", Service: "x"}
	tool := &domain.ToolDefinition{ID: "t1", Name: "get_next_deadline", FunctionName: "get_next_deadline", Action: "d", ReadOnly: readOnly, IntegrationID: integ.ID}
	skill := &domain.Skill{ID: "s1", Name: "deadlines", Description: "Deadlines", AgentID: "a1", Integrations: []string{integ.ID}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("x", lk)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.ToolsByID = map[string]*domain.ToolDefinition{"t1": tool}
		d.IntegrationsByID[integ.ID] = integ
	})
	host := runtime.NewHost()
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: deps})
	return &wakeRig{host: host, llm: llm, lookup: lk, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
}

func (r *wakeRig) plan(due bool, last *runtime.WakeRecord) (runtime.WakePlan, error) {
	return r.host.PlanWake(context.Background(), runtime.WakeRequest{
		AgentID: "a1", UserID: "u1", Action: "Look at my squad and tell me who to drop",
		Timing: "two hours before the squad deadline", Zone: "Europe/London", Now: r.now, Last: last, Due: due,
	})
}

func lookUp(id string) func([]domain.Message) []domain.ToolCall {
	return func([]domain.Message) []domain.ToolCall { return call(id, "get_next_deadline", map[string]any{}) }
}

func setWake(args map[string]any) func([]domain.Message) []domain.ToolCall {
	return func([]domain.Message) []domain.ToolCall { return call("w", "set_next_wake", args) }
}

func finish() func([]domain.Message) []domain.ToolCall {
	return func([]domain.Message) []domain.ToolCall {
		return call("f", domain.FinishActionName, map[string]any{"content": "ok"})
	}
}

// The agent looks the moment up with its own tools, works out the time, and says why: the code knows
// nothing about the thing the person named.
func TestPlanWake_TimeComesFromALookUpAndCarriesItsReason(t *testing.T) {
	r := newWakeRig(t, true, lookUp("l1"), setWake(map[string]any{
		"at": "2026-10-25T08:30:00Z", "kind": "act", "reason": "The deadline is Saturday 10:30, so two hours before is 8:30.",
	}), finish())
	plan, err := r.plan(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 25, 8, 30, 0, 0, time.UTC); !plan.At.Equal(want) || plan.Kind != runtime.WakeAct || plan.DoItNow {
		t.Fatalf("plan = %+v", plan)
	}
	if !strings.Contains(plan.Reason, "deadline") {
		t.Errorf("the reason is kept: %q", plan.Reason)
	}
	if r.lookup.count() != 1 {
		t.Errorf("it looked the time up with its tool: %d", r.lookup.count())
	}
}

// A time that was not looked up is refused, however sure the agent is; it must look and try again.
func TestPlanWake_ATimeFromMemoryIsRefused(t *testing.T) {
	good := map[string]any{"at": "2026-10-25T08:30:00Z", "kind": "act", "reason": "From the deadline."}
	r := newWakeRig(t, true, setWake(good), lookUp("l1"), setWake(good), finish())
	plan, err := r.plan(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.At.IsZero() || r.lookup.count() != 1 {
		t.Fatalf("it had to look it up before the time was taken: plan=%+v lookups=%d", plan, r.lookup.count())
	}
	if !strings.Contains(lastToolResults(r.llm), "Noted") {
		t.Errorf("accepted only after the look-up: %q", lastToolResults(r.llm))
	}
}

func lastToolResults(l *scriptedLLM) string {
	var b strings.Builder
	for _, m := range l.saw {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

// Without a look-up that can ever work (nothing read-only to ask), it says so and sets nothing.
func TestPlanWake_NothingToLookUpWithSaysSo(t *testing.T) {
	r := newWakeRig(t, false, finish())
	plan, err := r.plan(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Say == "" || !plan.At.IsZero() {
		t.Fatalf("it should say it can't, and plan nothing: %+v", plan)
	}
	if r.llm.n != 0 {
		t.Errorf("no model call is wasted when nothing could settle it: %d", r.llm.n)
	}
}

func TestPlanWake_CannotPlanIsPassedOnInPlainWords(t *testing.T) {
	r := newWakeRig(t, true, lookUp("l1"), func([]domain.Message) []domain.ToolCall {
		return call("c", "cannot_plan", map[string]any{"message": "I couldn't find the next deadline. Do you know it?"})
	}, finish())
	plan, err := r.plan(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Say != "I couldn't find the next deadline. Do you know it?" || !plan.At.IsZero() {
		t.Fatalf("plan = %+v", plan)
	}
}

// Times the code will not take: too soon, too far, a check closer than an hour, or not a time at all.
// Each refusal is returned to the agent, which corrects itself, so the first bad try never lands.
func TestPlanWake_BadTimesAreRefusedAndTheAgentCorrectsThem(t *testing.T) {
	r := newWakeRig(t, true)
	good := map[string]any{"at": "2026-10-25T08:30:00Z", "kind": "act", "reason": "From the deadline."}
	cases := map[string]map[string]any{
		"in the past":       {"at": "2026-10-20T08:30:00Z", "kind": "act", "reason": "x"},
		"too soon for act":  {"at": "2026-10-21T12:05:00Z", "kind": "act", "reason": "x"},
		"check within hour": {"at": "2026-10-21T12:30:00Z", "kind": "check", "reason": "x"},
		"over a week away":  {"at": "2026-11-20T08:30:00Z", "kind": "act", "reason": "x"},
		"not a time":        {"at": "after the deadline", "kind": "act", "reason": "x"},
		"no reason":         {"at": "2026-10-25T08:30:00Z", "kind": "act", "reason": " "},
		"unknown kind":      {"at": "2026-10-25T08:30:00Z", "kind": "maybe", "reason": "x"},
	}
	for name, bad := range cases {
		r = newWakeRig(t, true, lookUp("l1"), setWake(bad), setWake(good), finish())
		plan, err := r.plan(false, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := time.Date(2026, 10, 25, 8, 30, 0, 0, time.UTC); !plan.At.Equal(want) {
			t.Errorf("%s: the bad time must not be kept, got %v", name, plan.At)
		}
	}
}

func confirmMoment() func([]domain.Message) []domain.ToolCall {
	return func([]domain.Message) []domain.ToolCall { return call("c", "confirm_moment", map[string]any{}) }
}

// At an act wake-up the agent may say the moment has come only after looking again and only on
// time; and it gives the next wake-up as well.
func TestPlanWake_ConfirmingTheMomentOnlyCountsAtAnOnTimeActWakeUpAfterLookingAgain(t *testing.T) {
	last := &runtime.WakeRecord{At: time.Date(2026, 10, 21, 11, 59, 0, 0, time.UTC), Kind: runtime.WakeAct, Reason: "8:30"}
	next := map[string]any{"at": "2026-10-28T08:30:00Z", "kind": "act", "reason": "Next deadline."}

	r := newWakeRig(t, true, lookUp("l1"), confirmMoment(), setWake(next), finish())
	plan, err := r.plan(true, last)
	if err != nil || !plan.DoItNow || !plan.At.Equal(time.Date(2026, 10, 28, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("on time after looking again: %+v %v", plan, err)
	}

	// Confirming before looking is refused; once it has looked it counts.
	r = newWakeRig(t, true, confirmMoment(), lookUp("l1"), confirmMoment(), setWake(next), finish())
	if plan, err = r.plan(true, last); err != nil || !plan.DoItNow {
		t.Fatalf("it must look again before the moment can be confirmed, then it can: %+v %v", plan, err)
	}
	r = newWakeRig(t, true, confirmMoment(), setWake(next), lookUp("l1"), setWake(next), finish())
	if plan, err = r.plan(true, last); err != nil || plan.DoItNow {
		t.Fatalf("a confirmation with no look-up behind it never counts: %+v %v", plan, err)
	}

	// Not at a wake-up, at a look-again wake-up, or too late: the action is not even offered.
	chk := &runtime.WakeRecord{At: last.At, Kind: runtime.WakeCheck, Reason: "look again"}
	late := &runtime.WakeRecord{At: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), Kind: runtime.WakeAct, Reason: "8:30"}
	for name, c := range map[string]struct {
		due  bool
		last *runtime.WakeRecord
	}{"a first plan": {false, nil}, "a look-again wake-up": {true, chk}, "too late": {true, late}} {
		r = newWakeRig(t, true, lookUp("l1"), confirmMoment(), setWake(next), finish())
		if plan, err = r.plan(c.due, c.last); err != nil || plan.DoItNow {
			t.Errorf("%s must never be done now: %+v %v", name, plan, err)
		}
	}
}

// The agent that never settles on a time is an error to retry, not a task to run.
func TestPlanWake_NoDecisionIsAnError(t *testing.T) {
	r := newWakeRig(t, true, lookUp("l1"), finish())
	if _, err := r.plan(false, nil); err == nil {
		t.Fatal("no wake-up set and nothing said must be an error")
	}
}
