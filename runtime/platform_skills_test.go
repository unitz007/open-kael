package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// skillListRouter answers like intentRouter and records the names of the capabilities it was asked to pick from.
type skillListRouter struct {
	intentRouter
	lists [][]string
}

func (r *skillListRouter) note(skills []*domain.Skill) {
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	r.lists = append(r.lists, names)
}
func (r *skillListRouter) PickSkills(ctx context.Context, text string, skills []*domain.Skill) ([]string, domain.Intent, error) {
	r.note(skills)
	return r.intentRouter.PickSkills(ctx, text, skills)
}
func (r *skillListRouter) PickSkillsInContext(ctx context.Context, text string, _ domain.Earlier, skills []*domain.Skill) ([]string, domain.Intent, error) {
	return r.PickSkills(ctx, text, skills)
}

const alertsName = "platform_alerts"

func platformRig(t *testing.T, text string, intent domain.Intent, picks []string, llm domain.LLM) (*followRig, *skillListRouter, *int) {
	t.Helper()
	router := &skillListRouter{intentRouter: intentRouter{
		intents:      map[string]domain.Intent{text: intent},
		followRouter: followRouter{byText: map[string][]string{text: picks}},
	}}
	r := newFollowRig(t, router)
	r.hosted.Agent.Description = "Forex trading desk"
	r.hosted.Agent.LLMs = []domain.LLM{llm}
	ran := 0
	r.host.SetPlatformSkills(func(ctx context.Context, _ *runtime.HostedAgent) []runtime.PlatformSkill {
		return []runtime.PlatformSkill{{
			Skill: &domain.Skill{ID: "platform:alerts", Name: alertsName, Description: "Tells the person when something happens: a new trading signal was made"},
			Actions: []*domain.BoundAction{{
				Spec:   domain.ActionSpec{Name: "set_up_reaction", Description: "Set up an alert"},
				Invoke: func(context.Context, map[string]any) (any, error) { ran++; return "set up", nil },
			}},
		}}
	})
	return r, router, &ran
}

// The router is asked to pick from the agent's own capabilities and the platform's, so "let me know whenever …" can
// match Alerts by meaning like anything else.
func TestPlatformSkill_TheRouterCanPickIt(t *testing.T) {
	llm := &alertLLM{call: "set_up_reaction", reply: "Done."}
	r, router, ran := platformRig(t, "Let me know whenever there's a new signal", domain.IntentExecute, []string{alertsName}, llm)
	withAnotherCapability(r) // with one, nothing is picked (TestPlatformSkill_AMinionWithOneCapabilityStillHasIt)
	r.say(t, "C1", "Let me know whenever there's a new signal")

	if len(router.lists) != 1 || strings.Join(router.lists[0], ",") != "analyse_pair,keep_journal,"+alertsName {
		t.Fatalf("the router must see the agent's capabilities and then the platform's: %v", router.lists)
	}
	if *ran != 1 {
		t.Fatalf("the alert action must run: %d", *ran)
	}
	got := strings.Join(llm.offered[0], ",")
	if strings.Count(got, "set_up_reaction") != 1 {
		t.Errorf("the platform action is offered once, never twice: %q", got)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != "Done." {
		t.Errorf("its reply is the turn's: %q", got)
	}
	if len(r.hosted.Agent.Skills) != 2 {
		t.Errorf("the agent's own capabilities are never changed: %d", len(r.hosted.Agent.Skills))
	}
}

// A minion (one capability of its own) isn't put through the picker, and the platform's capabilities still come with
// every turn: "let me know whenever …" sets the alert up.
func TestPlatformSkill_AMinionWithOneCapabilityStillHasIt(t *testing.T) {
	llm := &alertLLM{call: "set_up_reaction", reply: "Done."}
	r, router, ran := platformRig(t, "Let me know whenever there's a new signal", domain.IntentExecute, nil, llm)
	r.say(t, "C1", "Let me know whenever there's a new signal")
	if len(router.lists) != 0 {
		t.Errorf("nothing to pick between: %v", router.lists)
	}
	if *ran != 1 {
		t.Fatalf("the alert action must run: %d", *ran)
	}
	if got := strings.Join(llm.offered[0], ","); strings.Count(got, "set_up_reaction") != 1 || !strings.Contains(got, "get_live_quote") {
		t.Errorf("its abilities and the alert action, once: %q", got)
	}
}

// "Can you tell me when …" may read as a question, where only look-ups are offered. The platform's actions carry their
// own confirmation, so they are still offered.
func TestPlatformSkill_AQuestionStillReachesItsActions(t *testing.T) {
	llm := &alertLLM{call: "set_up_reaction", reply: "Done."}
	r, _, ran := platformRig(t, "Can you tell me when a new signal comes in?", domain.IntentRecommend, []string{alertsName}, llm)
	r.say(t, "C1", "Can you tell me when a new signal comes in?")
	if *ran != 1 {
		t.Fatalf("the alert action must be reachable on a question: offered %v", llm.offered)
	}
}

// Picked beside one of the agent's own capabilities, both sets of actions are there.
func TestPlatformSkill_PickedBesideAnotherCapability(t *testing.T) {
	r, _, _ := platformRig(t, "When a new signal comes in, analyse it", domain.IntentExecute, []string{"analyse_pair", alertsName}, &alertLLM{reply: "ok"})
	spy := &toolSpy{}
	r.hosted.Agent.LLMs = []domain.LLM{spy}
	r.say(t, "C1", "When a new signal comes in, analyse it")
	got := strings.Join(spy.offered[0], ",")
	for _, want := range []string{"get_live_quote", "set_up_reaction"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s must be offered: %q", want, got)
		}
	}
}

// A message that matches only the agent's own capability still has the platform's actions, as extra actions had.
func TestPlatformSkill_ItsActionsAreThereOnOtherTurnsToo(t *testing.T) {
	spy := &toolSpy{}
	r, _, _ := platformRig(t, "Check audusd", domain.IntentExecute, []string{"analyse_pair"}, spy)
	r.say(t, "C1", "Check audusd")
	if got := strings.Join(spy.offered[0], ","); !strings.Contains(got, "set_up_reaction") {
		t.Errorf("offered on a person's routed turn: %q", got)
	}
}

// An unattended run (a repeating task, an alert being carried out) never gets the platform's capabilities.
func TestPlatformSkill_NeverOnAnUnattendedRun(t *testing.T) {
	r, router, _ := platformRig(t, "Check audusd", domain.IntentRecommend, []string{"analyse_pair"}, &toolSpy{})
	ctx := domain.WithUnattended(context.Background())
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	if _, err := r.host.HandleTurn(ctx, r.hosted, conv, "Check audusd"); err != nil {
		t.Fatal(err)
	}
	for _, l := range router.lists {
		if strings.Contains(strings.Join(l, ","), alertsName) {
			t.Fatalf("not offered to the router on an unattended run: %v", l)
		}
	}
}

// A message that matches nothing, not even the platform's capability, still gets the fixed "here's what I can do"
// without a model call.
func TestPlatformSkill_AnUnmatchedMessageStillGetsTheFixedReply(t *testing.T) {
	spy := &toolSpy{}
	r, _, ran := platformRig(t, "book me a flight", domain.IntentExecute, nil, spy)
	withAnotherCapability(r)
	r.say(t, "C1", "book me a flight")
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Fatalf("fixed reply: %q", got)
	}
	if spy.calls() != 0 || *ran != 0 {
		t.Fatalf("no model, no alert: calls=%d ran=%d", spy.calls(), *ran)
	}
}
