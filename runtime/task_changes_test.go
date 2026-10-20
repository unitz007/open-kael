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

// changeRig is an agent whose one capability can look things up (read_thing) and change things
// three ways: create_event, delete_event, and send_email, which is set to ask for approval.
type changeRig struct {
	host    *runtime.Host
	box     *outbox
	fx      *fakeExecutor
	llm     *offerLLM
	require *fakeRequirer
}

// offerLLM records what it was offered and runs a script: call create_event first (when told to),
// then finish, saying it is done.
type offerLLM struct {
	mu      sync.Mutex
	offered [][]string
	callOne string // the action to call first ("" = just finish)
}

func (o *offerLLM) Call(_ context.Context, msgs []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var names []string
	for _, a := range actions {
		names = append(names, a.Name)
	}
	o.offered = append(o.offered, names)
	last := msgs[len(msgs)-1]
	if o.callOne != "" && last.Role != domain.RoleTool {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "c", Name: o.callOne, Arguments: map[string]any{}}}}, nil
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": "All booked.", "outcome": "done"}}}}, nil
}

func newChangeRig(t *testing.T, requires []string) *changeRig {
	t.Helper()
	box := &outbox{}
	fx := &fakeExecutor{}
	integ := &domain.Integration{ID: "int-cal", Service: "cal"}
	def := func(id, name string, readOnly, approval bool) *domain.ToolDefinition {
		return &domain.ToolDefinition{ID: id, Name: name, FunctionName: name, Action: name, ReadOnly: readOnly, RequiresApproval: approval, IntegrationID: integ.ID}
	}
	tools := map[string]*domain.ToolDefinition{
		"t_read":   def("t_read", "read_thing", true, false),
		"t_create": def("t_create", "create_event", false, false),
		"t_delete": def("t_delete", "delete_event", false, false),
		"t_send":   def("t_send", "send_email", false, true),
	}
	skill := &domain.Skill{ID: "s1", Name: "calendar", Description: "Your calendar", AgentID: "a1", Integrations: []string{integ.ID}}
	llm := &offerLLM{}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 6, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	executors.Register("cal", fx)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
		d.ToolsByID = tools
		d.IntegrationsByID[integ.ID] = integ
	})
	const task = "Book my gym sessions for the week"
	router := &intentRouter{intents: map[string]domain.Intent{task: domain.IntentExecute}, pickIntent: domain.IntentExecute}
	router.byText = map[string][]string{task: {"calendar"}}
	require := &fakeRequirer{names: requires}
	host := runtime.NewHost()
	host.SetSkillRouter(router)
	host.SetToolRequirer(require)
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: deps})
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	return &changeRig{host: host, box: box, fx: fx, llm: llm, require: require}
}

const gymTask = "Book my gym sessions for the week"

func TestPlanTaskChanges_NamesWhatATaskNeedsAndSetsApartWhatNeedsApproval(t *testing.T) {
	r := newChangeRig(t, []string{"send_email", "create_event"})
	plan, err := r.host.PlanTaskChanges(context.Background(), "a1", "u1", gymTask)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(plan.Allowed, ",") != "create_event" || strings.Join(plan.NeedsApproval, ",") != "send_email" {
		t.Errorf("an action that asks for approval can never be granted: %+v", plan)
	}
	// Only change-making actions were put to the question; look-ups are not what it grants.
	if joined := strings.Join(r.require.passed, ","); strings.Contains(joined, "read_thing") || !strings.Contains(joined, "create_event") {
		t.Errorf("the router is asked which CHANGING actions the task needs: %s", joined)
	}
}

func TestPlanTaskChanges_APersonsOwnApprovalSettingHolds(t *testing.T) {
	r := newChangeRig(t, []string{"create_event"})
	r.host.SetUserToolApprovalLoader(func(context.Context, string) (map[string]bool, error) { return map[string]bool{"t_create": true}, nil })
	plan, err := r.host.PlanTaskChanges(context.Background(), "a1", "u1", gymTask)
	if err != nil || len(plan.Allowed) != 0 || strings.Join(plan.NeedsApproval, ",") != "create_event" {
		t.Fatalf("they asked to be asked first about this action, so nobody-there can't do it: %+v %v", plan, err)
	}
}

func TestPlanTaskChanges_NothingIsGrantedOnAGuess(t *testing.T) {
	// The router can't say which actions fit.
	r := newChangeRig(t, nil)
	r.require.err = errors.New("down")
	if _, err := r.host.PlanTaskChanges(context.Background(), "a1", "u1", gymTask); !errors.Is(err, runtime.ErrCannotPlan) {
		t.Errorf("router failure: %v", err)
	}
	// Their approval settings can't be read.
	r2 := newChangeRig(t, []string{"create_event"})
	r2.host.SetUserToolApprovalLoader(func(context.Context, string) (map[string]bool, error) { return nil, errors.New("db down") })
	if _, err := r2.host.PlanTaskChanges(context.Background(), "a1", "u1", gymTask); !errors.Is(err, runtime.ErrCannotPlan) {
		t.Errorf("can't see their settings: %v", err)
	}
	// No router that decides which actions are needed at all.
	bare := runtime.NewHost()
	bare.Register(&runtime.HostedAgent{Agent: &domain.Agent{ID: "a1"}, Deps: slackDeps()})
	if _, err := bare.PlanTaskChanges(context.Background(), "a1", "u1", gymTask); !errors.Is(err, runtime.ErrCannotPlan) {
		t.Errorf("no router: %v", err)
	}
	if _, err := r.host.PlanTaskChanges(context.Background(), "ghost", "u1", gymTask); err == nil {
		t.Error("an agent that isn't there")
	}
}

// What the person agreed to, and no more: the task can look things up and use the actions it was
// allowed to change things with. Another action that could change something is not even offered.
func TestScheduledTask_MayChangeOnlyWhatThePersonAgreedTo(t *testing.T) {
	r := newChangeRig(t, []string{"create_event"})
	r.llm.callOne = "create_event"
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", gymTask, "create_event"); err != nil {
		t.Fatal(err)
	}
	got := waitTexts(r.box, 1)
	if len(got) != 1 || !strings.Contains(got[0], "All booked.") {
		t.Fatalf("it tells them what it did: %q", got)
	}
	offered := strings.Join(r.llm.offered[0], ",")
	if !strings.Contains(offered, "read_thing") || !strings.Contains(offered, "create_event") {
		t.Errorf("a look-up and the allowed change are offered: %s", offered)
	}
	for _, never := range []string{"delete_event", "send_email", runtime.AskOptionsActionName} {
		if strings.Contains(offered, never) {
			t.Errorf("%q must not be offered to a task that was only allowed create_event: %s", never, offered)
		}
	}
	if calls := r.fx.calls(); len(calls) != 1 || calls[0].action != "create_event" {
		t.Errorf("the allowed change really ran: %+v", calls)
	}
}

// An action that asks for approval runs unattended only when the person named it when they set the task up: they
// were shown it and agreed, and that agreement stands in for approving each use. Anything not named still fails
// closed, because there is no one to approve it.
func TestScheduledTask_AnActionThatNeedsApprovalRunsOnlyIfThePersonAgreedToItByName(t *testing.T) {
	agreed := newChangeRig(t, []string{"send_email"})
	agreed.llm.callOne = "send_email"
	if err := agreed.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", gymTask, "send_email"); err != nil {
		t.Fatal(err)
	}
	waitTexts(agreed.box, 1)
	if calls := agreed.fx.calls(); len(calls) != 1 {
		t.Fatalf("what the person agreed to by name runs, without anyone to approve it: %+v", calls)
	}

	other := newChangeRig(t, []string{"send_email"})
	other.llm.callOne = "send_email"
	if err := other.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", gymTask, "create_event"); err != nil {
		t.Fatal(err)
	}
	waitTexts(other.box, 1)
	if calls := other.fx.calls(); len(calls) != 0 {
		t.Fatalf("an action that asks first and was not named never runs unattended: %+v", calls)
	}
}

// With nothing agreed to, the same task is a look-up only (as before).
func TestScheduledTask_WithNoAgreementItStillOnlyLooksThingsUp(t *testing.T) {
	r := newChangeRig(t, []string{"create_event"})
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", gymTask); err != nil {
		t.Fatal(err)
	}
	waitTexts(r.box, 1)
	offered := strings.Join(r.llm.offered[0], ",")
	if !strings.Contains(offered, "read_thing") || strings.Contains(offered, "create_event") || strings.Contains(offered, "delete_event") {
		t.Errorf("look-ups only: %s", offered)
	}
}
