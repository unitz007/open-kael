package runtime_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// unattendedRig is an agent with one capability that can both look something up and change
// something, behind a messenger that CAN show questions (so the test proves they aren't offered).
type unattendedRig struct {
	host   *runtime.Host
	box    *choosingExecutor
	spy    *toolSpy
	mem    *memMemory
	router *intentRouter
}

func newUnattendedRig(t *testing.T, intent domain.Intent, task string) *unattendedRig {
	t.Helper()
	spy := &toolSpy{}
	box := &choosingExecutor{}
	mem := newMemMemory()
	integ := &domain.Integration{ID: "int-fx", Service: "fx"}
	read := &domain.ToolDefinition{ID: "t_read", Name: "read_thing", FunctionName: "read_thing", Action: "r", ReadOnly: true, IntegrationID: integ.ID}
	change := &domain.ToolDefinition{ID: "t_change", Name: "change_thing", FunctionName: "change_thing", Action: "c", IntegrationID: integ.ID}
	skill := &domain.Skill{ID: "s1", Name: "calendar", Description: "Your calendar", AgentID: "a1", Integrations: []string{integ.ID}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{spy}, MaxIterations: 5, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	executors.Register("fx", &fakeExecutor{})
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = mem
		d.ToolsByID = map[string]*domain.ToolDefinition{"t_read": read, "t_change": change}
		d.IntegrationsByID[integ.ID] = integ
	})
	router := &intentRouter{intents: map[string]domain.Intent{task: intent}}
	router.byText = map[string][]string{task: {"calendar"}}
	router.pickIntent = intent
	host := runtime.NewHost()
	host.SetSkillRouter(router)
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: deps})
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	return &unattendedRig{host: host, box: box, spy: spy, mem: mem, router: router}
}

const checkTask = "Check my calendar and send me a summary of what is on this week"

// A repeating task can look things up and report, however the stored words sound: even a task that
// would normally be a change is carried out as a look-up, with no way to ask the person a question.
func TestScheduledTask_OnlyLooksThingsUpAndNeverAsksAnything(t *testing.T) {
	r := newUnattendedRig(t, domain.IntentExecute, checkTask)
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", checkTask); err != nil {
		t.Fatal(err)
	}
	got := waitTexts(&r.box.outbox, 1)
	if len(got) != 1 || got[0] != "ok" {
		t.Fatalf("the reply is delivered to the person's chat: %q", got)
	}
	offered := strings.Join(r.spy.offered[0], ",")
	if !strings.Contains(offered, "read_thing") {
		t.Errorf("a look-up is available: %s", offered)
	}
	for _, never := range []string{"change_thing", runtime.AskOptionsActionName} {
		if strings.Contains(offered, never) {
			t.Errorf("an unattended run is never offered %q: %s", never, offered)
		}
	}
}

// The task text is the stored instruction, not something the person wrote just now: only the
// reply goes into their conversation, so a follow-up has it to refer to.
func TestScheduledTask_LeavesOnlyItsReplyInTheConversation(t *testing.T) {
	r := newUnattendedRig(t, domain.IntentRecommend, checkTask)
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", checkTask); err != nil {
		t.Fatal(err)
	}
	waitTexts(&r.box.outbox, 1)
	var stored []domain.Message
	for _, m := range r.mem.store {
		stored = m
	}
	if len(stored) != 1 || stored[0].Role != domain.RoleAssistant || stored[0].Content != "ok" {
		t.Fatalf("only the reply is kept: %+v", stored)
	}
}

// A task is never the place to set up another repeating task: it can't loop on itself.
func TestScheduledTask_NeverSetsUpAnotherRepeatingTask(t *testing.T) {
	r := newUnattendedRig(t, domain.IntentSchedule, checkTask)
	var calls atomic.Int32
	r.host.SetIntentHandler(domain.IntentSchedule, func(context.Context, string, string, string, string, string, string) (string, error) {
		calls.Add(1)
		return "scheduled", nil
	})
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", checkTask); err != nil {
		t.Fatal(err)
	}
	got := waitTexts(&r.box.outbox, 1)
	if calls.Load() != 0 || len(got) != 1 || got[0] != "ok" {
		t.Fatalf("the schedule handler must not run for an unattended task: calls=%d %q", calls.Load(), got)
	}
}

// The same message from the person, in a chat, is a normal turn: it can be offered a question and a change.
func TestScheduledTask_ANormalTurnIsNotRestrictedTheSameWay(t *testing.T) {
	r := newUnattendedRig(t, domain.IntentExecute, checkTask)
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
	if _, err := r.host.HandleTurn(context.Background(), hostedOf(t, r.host, "a1"), conv, checkTask); err != nil {
		t.Fatal(err)
	}
	offered := strings.Join(r.spy.offered[0], ",")
	if !strings.Contains(offered, "change_thing") || !strings.Contains(offered, runtime.AskOptionsActionName) {
		t.Errorf("an attended turn can change things and ask: %s", offered)
	}
}

func TestScheduledTask_RefusesWhatCannotRun(t *testing.T) {
	host := runtime.NewHost()
	if err := host.RunScheduledTask(context.Background(), "ghost", "u1", "id_slack", "C1", "do it"); err == nil {
		t.Error("an agent that isn't there can't run a task")
	}
	r := newUnattendedRig(t, domain.IntentRecommend, checkTask)
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "   "); err == nil {
		t.Error("an empty task is not run")
	}
}

func hostedOf(t *testing.T, h *runtime.Host, id string) *runtime.HostedAgent {
	t.Helper()
	hosted, ok := h.Get(id)
	if !ok {
		t.Fatalf("no agent %q", id)
	}
	return hosted
}

// A handler that already said everything on screen (it showed buttons, and the answer is there)
// returns nothing, and then nothing more is sent: no empty message, no stray model call.
func TestIntentHandler_AnEmptyReplySendsNothingAndAReplyIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		reply string
		want  []string
	}{{"", nil}, {"   ", nil}, {"hello", []string{"hello"}}} {
		r := newUnattendedRig(t, domain.IntentSchedule, checkTask)
		reply := tc.reply
		r.host.SetIntentHandler(domain.IntentSchedule, func(context.Context, string, string, string, string, string, string) (string, error) {
			return reply, nil
		})
		conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
		if _, err := r.host.HandleTurn(context.Background(), hostedOf(t, r.host, "a1"), conv, checkTask); err != nil {
			t.Fatal(err)
		}
		got := r.box.texts()
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("reply %q: sent %q, want %q", tc.reply, got, tc.want)
		}
		if r.spy.calls() != 0 {
			t.Errorf("reply %q: the model must not be called after a handler answered: %d", tc.reply, r.spy.calls())
		}
	}
}
