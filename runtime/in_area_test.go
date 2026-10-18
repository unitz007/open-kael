package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// scopeRouter matches no ability and judges scope from a script.
type scopeRouter struct {
	followRouter
	inScope map[string]bool
	err     error
	asked   []string // what it was shown as the agent's area
}

func (r *scopeRouter) IsInScope(_ context.Context, text, agentContext string) (bool, error) {
	r.asked = append(r.asked, agentContext)
	return r.inScope[text], r.err
}

// "Is the market open?" to a forex agent matched no ability, so it was told "I can't help
// with that one". A question in the agent's own area is answered, with no tools.
func TestInArea_AQuestionInTheAgentsAreaIsAnsweredWithoutTools(t *testing.T) {
	router := &scopeRouter{inScope: map[string]bool{"Is the market open?": true}}
	r := newFollowRig(t, router)
	r.hosted.Agent.Description = "Forex trading desk"
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
	// The router judged the message against what the agent is for, not against its list of
	// abilities (which it reads too narrowly).
	if len(router.asked) != 1 || router.asked[0] != "Forex trading desk" {
		t.Errorf("scope must be judged against the agent's description alone: %q", router.asked)
	}
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			if m.Role == domain.RoleSystem {
				t.Errorf("the instruction that shaped the reply must not be stored: %q", m.Content)
			}
		}
	}
}

// Off topic is still declined in fixed words, with no model call.
func TestInArea_AnOffTopicMessageIsStillDeclinedInFixedWords(t *testing.T) {
	r := newFollowRig(t, &scopeRouter{inScope: map[string]bool{}})
	r.hosted.Agent.Description = "Forex trading desk"
	r.say(t, "C1", "what's the weather like")
	want := "I can't help with that one. Here's what I can do:\n• Analyses a currency pair\n\nWhat would you like to try?"
	if got := r.box.texts(); len(got) != 1 || got[0] != want {
		t.Fatalf("fixed decline:\n got %q\nwant %q", got, want)
	}
	if r.spy.calls() != 0 {
		t.Fatalf("no model is asked: %d calls", r.spy.calls())
	}
}

// If the router can't say whether it is in scope, the answer stays the safe one.
func TestInArea_WhenScopeCannotBeJudgedItIsTheFixedDecline(t *testing.T) {
	r := newFollowRig(t, &scopeRouter{err: errors.New("jev down"), inScope: map[string]bool{"Is the market open?": true}})
	r.hosted.Agent.Description = "Forex trading desk"
	r.say(t, "C1", "Is the market open?")
	if got := r.box.texts(); len(got) != 1 || !strings.HasPrefix(got[0], "I can't help with that one.") {
		t.Fatalf("fixed decline expected: %q", got)
	}
	if r.spy.calls() != 0 {
		t.Fatalf("no model is asked: %d calls", r.spy.calls())
	}
}

// The model is told not to pretend, and the hint says so in the words the person never sees.
func TestInArea_TheModelIsToldNotToPretendToLookThingsUp(t *testing.T) {
	var seen string
	spy := &promptSpy{seen: &seen}
	router := &scopeRouter{inScope: map[string]bool{"Is the market open?": true}}
	r := newFollowRig(t, router)
	r.hosted.Agent.Description = "Forex trading desk"
	r.hosted.Agent.LLMs = []domain.LLM{spy}
	r.say(t, "C1", "Is the market open?")
	for _, want := range []string{"none of your abilities covers", "Never say or imply that you are looking something up", "live prices"} {
		if !strings.Contains(seen, want) {
			t.Errorf("the model must be told %q: %q", want, seen)
		}
	}
}

// An agent with nothing written about what it is for can't be judged, so it keeps the safe reply.
func TestInArea_WithNoDescriptionItIsTheFixedDecline(t *testing.T) {
	router := &scopeRouter{inScope: map[string]bool{"Is the market open?": true}}
	r := newFollowRig(t, router)
	r.say(t, "C1", "Is the market open?")
	if got := r.box.texts(); len(got) != 1 || !strings.HasPrefix(got[0], "I can't help with that one.") || len(router.asked) != 0 {
		t.Fatalf("no description, no scope judgement: %q asked=%v", got, router.asked)
	}
}
