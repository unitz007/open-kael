package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// turnLog is a recorder that also keeps what each message was handled with, step by step.
type turnLog struct {
	activityLog
	turnsMu sync.Mutex
	turns   []domain.TurnRecord
}

func (l *turnLog) TurnHandled(_ context.Context, _ string, rec domain.TurnRecord) {
	l.turnsMu.Lock()
	l.turns = append(l.turns, rec)
	l.turnsMu.Unlock()
}

func (l *turnLog) wait(n int) []domain.TurnRecord {
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		l.turnsMu.Lock()
		got := append([]domain.TurnRecord(nil), l.turns...)
		l.turnsMu.Unlock()
		if len(got) >= n || time.Now().After(end) {
			return got
		}
	}
}

// While an agent works, its creator's screen can see which capability it is on and the abilities it has used so far
// (ids and how each went); afterwards the message is kept as one record. Platform actions such as replying are not
// abilities and never show; alerts and repeating tasks are not kept.
func TestSteps_WhatAnAgentIsDoingIsSeenLiveAndKeptAfterwards(t *testing.T) {
	router := &intentRouter{
		intents:      map[string]domain.Intent{"Check audusd": domain.IntentRecommend},
		followRouter: followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}},
	}
	r := newIntentRig(t, router)
	log := &turnLog{}
	r.host.SetActivityRecorder(log)
	var during map[string]domain.LiveTurn
	r.hosted.Agent.LLMs = []domain.LLM{&funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "get_live_quote", Arguments: map[string]any{}}}}, nil
		}
		during = r.host.LiveTurns([]string{"a1", "other"})
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "1.0412"}}}}, nil
	}}}

	r.say(t, "C1", "Check audusd")

	live, ok := during["a1"]
	if !ok || len(during) != 1 {
		t.Fatalf("only the agent at work is shown: %+v", during)
	}
	if len(live.Capabilities) != 1 || live.Capabilities[0] != "s1" || len(live.Steps) != 1 || live.Steps[0] != (domain.LiveStep{Tool: "q", State: domain.StepDone}) {
		t.Errorf("its capability and the ability it used, by id, with how it went: %+v", live)
	}
	if after := r.host.LiveTurns([]string{"a1"}); len(after) != 0 {
		t.Errorf("nothing is shown once it is done: %+v", after)
	}
	got := log.wait(1)
	if len(got) != 1 || got[0].Failed || len(got[0].Steps) != 1 || got[0].Steps[0].Tool != "q" || got[0].Capabilities[0] != "s1" {
		t.Fatalf("the message is kept with its steps: %+v", got)
	}

	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "Check audusd"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if after := log.wait(0); len(after) != 1 {
		t.Errorf("a repeating task is not a person's message, so it is not kept: %+v", after)
	}
}
