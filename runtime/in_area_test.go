package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
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
