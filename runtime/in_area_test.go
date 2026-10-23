package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// intentRouter classifies each message's intent FIRST, from a script, and records what it was
// asked. Its ability picker matches nothing and would answer "chat" for everything, to prove the
// first answer is the one that counts.
type intentRouter struct {
	followRouter
	intents    map[string]domain.Intent
	err        error
	areas      []string      // what the agent was said to be for, per question
	picked     int           // how many times abilities were looked for
	pickIntent domain.Intent // what the ability picker says the intent is (default: chat)
}

func (r *intentRouter) ClassifyIntent(_ context.Context, text string, _ domain.Earlier, area string) (domain.Intent, error) {
	r.areas = append(r.areas, area)
	if r.err != nil {
		return "", r.err
	}
	return r.intents[text], nil
}
func (r *intentRouter) PickSkills(_ context.Context, text string, _ []*domain.Skill) ([]string, domain.Intent, error) {
	r.picked++
	in := r.pickIntent
	if in == "" {
		in = domain.IntentChat
	}
	return r.byText[text], in, nil
}
func (r *intentRouter) PickSkillsInContext(ctx context.Context, text string, _ domain.Earlier, s []*domain.Skill) ([]string, domain.Intent, error) {
	return r.PickSkills(ctx, text, s)
}

func newIntentRig(t *testing.T, r *intentRouter) *followRig {
	t.Helper()
	rig := newFollowRig(t, r)
	rig.hosted.Agent.Description = "Forex trading desk"
	return rig
}

func fixedDecline() string {
	return "I can't help with that one. Here's what I can do:\n• Analyses a currency pair\n\nWhat would you like to try?"
}

// "Is the market open?" to a forex agent matched no ability, so it was told "I can't help with
// that one". It is a question, not an off-topic message: it is answered, with no tools.
func TestIntentFirst_AQuestionNoAbilityCoversIsAnsweredWithoutTools(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"Is the market open?": domain.IntentRecommend}}
	r := newIntentRig(t, router)
	r.say(t, "C1", "Is the market open?")

	if r.spy.calls() != 1 {
		t.Fatalf("the model answers once: %d calls", r.spy.calls())
	}
	if got := strings.Join(r.spy.offered[0], ","); got != domain.FinishActionName {
		t.Errorf("no tools, only the ability to reply: %q", got)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != "ok" {
		t.Errorf("the model's own reply is delivered, not the fixed decline: %q", got)
	}
	// The classifier was told what the agent is for, and the abilities were not part of that.
	if len(router.areas) != 1 || router.areas[0] != "Forex trading desk" {
		t.Errorf("the classifier must be told what the agent is for, from its description alone: %q", router.areas)
	}
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			if m.Role == domain.RoleSystem {
				t.Errorf("the instruction that shaped the reply must not be stored: %q", m.Content)
			}
		}
	}
}

// Unrelated to the agent: declined in fixed words, no model call, and abilities are never even
// looked for, because the intent already settled it.
func TestIntentFirst_AnOffTopicMessageIsDeclinedBeforeAbilitiesAreLookedFor(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"write me a poem": domain.IntentOffTopic}}
	r := newIntentRig(t, router)
	r.say(t, "C1", "write me a poem")
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Fatalf("fixed decline:\n got %q\nwant %q", got, fixedDecline())
	}
	if r.spy.calls() != 0 || router.picked != 0 {
		t.Fatalf("no model, no ability search: model=%d picked=%d", r.spy.calls(), router.picked)
	}
}

// Being friendly is its own intent, decided first: answered warmly, no tools, no ability search.
func TestIntentFirst_ASocialMessageIsAnsweredWarmlyWithoutLookingForAbilities(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"thanks!": domain.IntentChat}}
	r := newIntentRig(t, router)
	r.say(t, "C1", "thanks!")
	if r.spy.calls() != 1 || strings.Join(r.spy.offered[0], ",") != domain.FinishActionName {
		t.Fatalf("one reply, no tools: %d %v", r.spy.calls(), r.spy.offered)
	}
	if router.picked != 0 {
		t.Errorf("abilities are not looked for: %d", router.picked)
	}
}

// The first answer is the one that counts: the ability picker saying "chat" must not turn a
// real question into small talk.
func TestIntentFirst_TheClassifiedIntentOverridesWhatThePickerSays(t *testing.T) {
	router := &intentRouter{
		intents:      map[string]domain.Intent{"Check audusd": domain.IntentRecommend},
		followRouter: followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}},
	}
	r := newIntentRig(t, router)
	r.say(t, "C1", "Check audusd")
	if offered := strings.Join(r.spy.offered[0], ","); !strings.Contains(offered, "get_live_quote") {
		t.Fatalf("the matched ability's tools must be offered, not a chat reply: %v", r.spy.offered)
	}
}

// Something to DO that no ability covers is told what the agent can do, in fixed words.
func TestIntentFirst_ARequestToDoSomethingItCannotDoGetsTheFixedReply(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"book me a flight": domain.IntentExecute}}
	r := newIntentRig(t, router)
	r.say(t, "C1", "book me a flight")
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Fatalf("fixed reply expected: %q", got)
	}
	if r.spy.calls() != 0 {
		t.Fatalf("no model is asked: %d", r.spy.calls())
	}
}

// If the first question can't be answered, the router's own reading of the message is used, as
// before: the safe outcome, never a crash or silence.
func TestIntentFirst_WhenTheClassifierFailsTheOldReadingStillWorks(t *testing.T) {
	router := &intentRouter{err: errors.New("jev down"), pickIntent: domain.IntentRecommend, followRouter: followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}}}
	r := newIntentRig(t, router)
	r.say(t, "C1", "Check audusd")
	if offered := strings.Join(r.spy.offered[0], ","); !strings.Contains(offered, "get_live_quote") {
		t.Fatalf("falls back to matching abilities: %v", r.spy.offered)
	}
}

// With nothing written about what the agent is for, "unrelated" can't be judged: the classifier
// is told so (empty) and a question is still answered.
func TestIntentFirst_WithNoDescriptionTheClassifierIsToldThereIsNone(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"Is the market open?": domain.IntentRecommend}}
	r := newFollowRig(t, router) // no description
	r.say(t, "C1", "Is the market open?")
	if len(router.areas) != 1 || router.areas[0] != "" {
		t.Errorf("an empty area is passed as empty: %q", router.areas)
	}
}

// The model is told not to pretend.
func TestIntentFirst_TheModelIsToldNotToPretendToLookThingsUp(t *testing.T) {
	var seen string
	router := &intentRouter{intents: map[string]domain.Intent{"Is the market open?": domain.IntentRecommend}}
	r := newIntentRig(t, router)
	r.hosted.Agent.LLMs = []domain.LLM{&promptSpy{seen: &seen}}
	r.say(t, "C1", "Is the market open?")
	for _, want := range []string{"none of your abilities covers", "Never say or imply that you are looking something up", "live prices"} {
		if !strings.Contains(seen, want) {
			t.Errorf("the model must be told %q: %q", want, seen)
		}
	}
}

// promiseLLM answers each call with the next reply in its list (the last one repeats) and records
// the instructions the model was shown each time.
type promiseLLM struct {
	replies []string
	seen    [][]string
}

func (s *promiseLLM) Call(_ context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	var sys []string
	for _, m := range msgs {
		if m.Role == domain.RoleSystem {
			sys = append(sys, m.Content)
		}
	}
	s.seen = append(s.seen, sys)
	i := len(s.seen) - 1
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": s.replies[i]}}}}, nil
}

func inAreaRigWith(t *testing.T, llm domain.LLM) *followRig {
	t.Helper()
	router := &intentRouter{intents: map[string]domain.Intent{"Are there any tradeable pairs?": domain.IntentRecommend}}
	r := newIntentRig(t, router)
	r.hosted.Agent.LLMs = []domain.LLM{llm}
	return r
}

// Some models say "Let me check the major pairs…" with no tools to check anything: the promise is
// never kept. The question is asked once more, and what is sent is the honest answer.
func TestIntentFirst_APromisedLookUpWithNoToolsIsAskedAgain(t *testing.T) {
	llm := &promiseLLM{replies: []string{"Let me check the major pairs for tradeable setups right now.", "Any pair can be traded; I analyse one at a time, so tell me which."}}
	r := inAreaRigWith(t, llm)
	r.say(t, "C1", "Are there any tradeable pairs?")

	if got := r.box.texts(); len(got) != 1 || got[0] != "Any pair can be traded; I analyse one at a time, so tell me which." {
		t.Fatalf("the second answer is the one sent: %q", got)
	}
	if len(llm.seen) != 2 {
		t.Fatalf("asked twice: %d", len(llm.seen))
	}
	if joined := strings.Join(llm.seen[1], "\n"); !strings.Contains(joined, "Your last attempt said you were about to look something up") {
		t.Errorf("the second ask says what was wrong: %q", llm.seen[1])
	}
	for _, s := range llm.seen[0] {
		if strings.Contains(s, "last attempt") {
			t.Errorf("the first ask does not carry the correction: %q", s)
		}
	}
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			if m.Role == domain.RoleSystem || strings.Contains(m.Content, "Let me check") {
				t.Errorf("neither the instructions nor the broken promise are kept: %+v", m)
			}
		}
	}
}

func TestIntentFirst_AModelThatKeepsPromisingIsToldWhatTheAgentCanDoInstead(t *testing.T) {
	llm := &promiseLLM{replies: []string{"I'll scan the majors now.", "Let me run a scan for you."}}
	r := inAreaRigWith(t, llm)
	r.say(t, "C1", "Are there any tradeable pairs?")
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Errorf("what the agent can do, not a promise nothing keeps: %q", got)
	}
	if len(llm.seen) != 2 {
		t.Errorf("one retry, no more: %d", len(llm.seen))
	}
}

func TestIntentFirst_AnHonestAnswerIsNotAskedAgain(t *testing.T) {
	llm := &promiseLLM{replies: []string{"The forex market is open around the clock on weekdays, so yes."}}
	r := inAreaRigWith(t, llm)
	r.say(t, "C1", "Are there any tradeable pairs?")
	if len(llm.seen) != 1 || r.box.texts()[0] != llm.replies[0] {
		t.Errorf("one call, reply as given: %d %q", len(llm.seen), r.box.texts())
	}
}

// Only how a reply opens decides whether it promised something: a long, honest answer that happens to
// say "let me check" much later is left alone.
func TestIntentFirst_APhraseFarIntoALongAnswerIsNotAPromise(t *testing.T) {
	long := "Forex trades around the clock from Sunday evening to Friday evening. " + strings.Repeat("Liquidity is highest when London and New York overlap. ", 6) + "If you want a specific pair, let me check it for you."
	llm := &promiseLLM{replies: []string{long}}
	r := inAreaRigWith(t, llm)
	r.say(t, "C1", "Are there any tradeable pairs?")
	if len(llm.seen) != 1 || r.box.texts()[0] != long {
		t.Errorf("one call, reply as given: %d", len(llm.seen))
	}
}

// alertLLM calls the action named in call (when it is offered) and then finishes with reply; it records what it was
// offered each time.
type alertLLM struct {
	call, reply string
	offered     [][]string
	called      bool
}

func (s *alertLLM) Call(_ context.Context, _ []domain.Message, specs []domain.ActionSpec) (*domain.LLMResponse, error) {
	var names []string
	for _, sp := range specs {
		names = append(names, sp.Name)
	}
	s.offered = append(s.offered, names)
	if s.call != "" && !s.called && strings.Contains(strings.Join(names, ","), s.call) {
		s.called = true
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "a", Name: s.call, Arguments: map[string]any{}}}}, nil
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": s.reply}}}}, nil
}

func alertRig(t *testing.T, text string, intent domain.Intent, llm domain.LLM) (*followRig, *int) {
	t.Helper()
	r := newIntentRig(t, &intentRouter{intents: map[string]domain.Intent{text: intent}})
	r.hosted.Agent.LLMs = []domain.LLM{llm}
	ran := 0
	r.host.SetExtraActions(func(context.Context, *runtime.HostedAgent) []*domain.BoundAction {
		return []*domain.BoundAction{{
			Spec:   domain.ActionSpec{Name: "set_up_reaction", Description: "Set up an alert"},
			Invoke: func(context.Context, map[string]any) (any, error) { ran++; return "set up", nil },
		}}
	})
	return r, &ran
}

// The live incident: "Let me know whenever there's a new trading signal" matched no ability (alerts are not one), and
// the fixed "here's what I can do" was sent without the model ever seeing the alert actions.
func TestNoAbilityMatched_AnAlertRequestReachesTheAlertActions(t *testing.T) {
	llm := &alertLLM{call: "set_up_reaction", reply: "Done: I'll tell you about each new signal."}
	r, ran := alertRig(t, "Let me know whenever there's a new trading signal", domain.IntentExecute, llm)
	r.say(t, "C1", "Let me know whenever there's a new trading signal")
	if *ran != 1 {
		t.Fatalf("the alert action must run once: %d", *ran)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != llm.reply {
		t.Fatalf("its reply is the turn's: %q", got)
	}
	if got := strings.Join(llm.offered[0], ","); got != "set_up_reaction,"+domain.FinishActionName {
		t.Errorf("only the alert actions and finish are offered: %q", got)
	}
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			if m.Role == domain.RoleSystem {
				t.Errorf("the instruction that shaped the reply must not be stored: %q", m.Content)
			}
		}
	}
}

// Anything else that matches no ability is answered exactly as before: what the model wrote while it had only the
// alert actions is thrown away.
func TestNoAbilityMatched_SomethingElseStillGetsTheFixedReply(t *testing.T) {
	llm := &alertLLM{reply: "Sure, I'll book your flight!"}
	r, ran := alertRig(t, "book me a flight", domain.IntentExecute, llm)
	r.say(t, "C1", "book me a flight")
	if *ran != 0 {
		t.Fatalf("no alert action: %d", *ran)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Fatalf("the fixed reply, never the model's: %q", got)
	}
	if len(llm.offered) != 1 {
		t.Errorf("asked once: %d", len(llm.offered))
	}
}

// A question no ability covers still gets its own answer afterwards (asked a second time, with no tools).
func TestNoAbilityMatched_AQuestionIsStillAnsweredAfterwards(t *testing.T) {
	llm := &alertLLM{reply: "The market is open around the clock on weekdays."}
	r, ran := alertRig(t, "Is the market open?", domain.IntentRecommend, llm)
	r.say(t, "C1", "Is the market open?")
	if *ran != 0 || len(llm.offered) != 2 {
		t.Fatalf("no alert, then the usual answer: ran=%d calls=%d", *ran, len(llm.offered))
	}
	if got := strings.Join(llm.offered[1], ","); got != domain.FinishActionName {
		t.Errorf("the usual answer has no tools: %q", got)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != llm.reply {
		t.Errorf("one reply: %q", got)
	}
}
