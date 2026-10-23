package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// finisher finishes at once with the inputs it is given, and keeps what finish offered each time.
type finisher struct {
	mu      sync.Mutex
	args    map[string]any
	offered []domain.ActionSpec
}

func (f *finisher) Call(_ context.Context, _ []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range actions {
		if a.Name == domain.FinishActionName {
			f.offered = append(f.offered, a)
		}
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: f.args}}}, nil
}

func (f *finisher) asked(i int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.offered[i].InputSchema.Properties["no_ability_for_it"]
	return ok
}

func beyondRig(t *testing.T, args map[string]any) (*followRig, *finisher, *activityLog) {
	t.Helper()
	router := &intentRouter{
		intents:      map[string]domain.Intent{"Move my stop to break-even": domain.IntentRecommend, "Check audusd": domain.IntentRecommend, "Is the market open?": domain.IntentRecommend},
		followRouter: followRouter{byText: map[string][]string{"Move my stop to break-even": {"analyse_pair"}, "Check audusd": {"analyse_pair"}}},
	}
	r := newIntentRig(t, router)
	llm := &finisher{args: args}
	r.hosted.Agent.LLMs = []domain.LLM{llm}
	log := &activityLog{}
	r.host.SetActivityRecorder(log)
	r.host.SetTopicLabeler(func(context.Context, string) string { return "move a stop" })
	return r, llm, log
}

// A capability was picked ("analyse a pair" for "move my stop"), but what was asked is beyond it: the agent says so
// on finish, and it is counted under what people asked for that it can't do yet, as a topic, like a request that
// matched nothing. Found 2026-10-23: a tapped choice the agent couldn't carry out never reached the creator.
func TestBeyondAbilities_ARequestTheCapabilityCannotDoIsCounted(t *testing.T) {
	r, llm, log := beyondRig(t, map[string]any{"content": "I can't move stops, only analyse pairs.", "no_ability_for_it": true})
	r.say(t, "C1", "Move my stop to break-even")

	if !llm.asked(0) {
		t.Fatal("finish must ask whether it was beyond its abilities when a capability was picked")
	}
	if ev := log.waitEvents(1); len(ev) != 1 || ev[0] != "a1|unsupported|move a stop" {
		t.Fatalf("counted once, as a topic: %q", ev)
	}
}

// Done, answered, or anything else that is not "no ability for it" is not counted; nor is an answer that leaves it out.
func TestBeyondAbilities_WhatItCouldDoIsNotCounted(t *testing.T) {
	for _, args := range []map[string]any{
		{"content": "AUDUSD looks bullish.", "no_ability_for_it": false},
		{"content": "AUDUSD looks bullish."},
	} {
		r, _, log := beyondRig(t, args)
		r.say(t, "C1", "Check audusd")
		time.Sleep(100 * time.Millisecond)
		if _, ev := log.snapshot(); len(ev) != 0 {
			t.Errorf("%v: nothing counted: %q", args, ev)
		}
	}
}

// Only asked when a capability was picked: a question none covers is answered as before (and not counted), and an
// unattended run (a repeating task, an alert) is never asked: nobody asked it for anything.
func TestBeyondAbilities_OnlyAskedOnAPersonsTurnWithACapability(t *testing.T) {
	r, llm, log := beyondRig(t, map[string]any{"content": "It opens Sunday evening.", "no_ability_for_it": true})
	r.say(t, "C1", "Is the market open?")
	if llm.asked(0) {
		t.Error("a question no capability covers is not asked")
	}
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "Check audusd"); err != nil {
		t.Fatal(err)
	}
	n := 0
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		llm.mu.Lock()
		n = len(llm.offered)
		llm.mu.Unlock()
		if n >= 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("two runs: %d", n)
	}
	if llm.asked(1) {
		t.Error("an unattended run is not asked")
	}
	time.Sleep(100 * time.Millisecond)
	if _, ev := log.snapshot(); len(ev) != 0 {
		t.Errorf("nothing counted: %q", ev)
	}
}
