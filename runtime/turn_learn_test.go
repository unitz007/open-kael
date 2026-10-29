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

type heardTurns struct {
	mu    sync.Mutex
	turns []runtime.TurnLearn
}

func (h *heardTurns) LearnTurn(_ context.Context, t runtime.TurnLearn) error {
	h.mu.Lock()
	h.turns = append(h.turns, t)
	h.mu.Unlock()
	return nil
}

func (h *heardTurns) wait(t *testing.T, n int) []runtime.TurnLearn {
	t.Helper()
	for i := 0; i < 200; i++ {
		h.mu.Lock()
		got := append([]runtime.TurnLearn(nil), h.turns...)
		h.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("heard %d turns, want %d", len(h.turns), n)
	return nil
}

// A learner hears each exchange with the reply the person was answering, and the apps the reply used; what a prompt
// note adds reaches the agent's instructions for that person.
func TestTurnLearner_HearsTheExchangeAndNotesReachThePrompt(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(domain.ToolCall{ID: "q", Name: "get_live_quote", Arguments: map[string]any{}})
		case 2:
			return calls(finishCall("EURUSD is 1.0850. Shall I buy 1 lot?", ""))
		}
		return calls(finishCall("Half a lot then.", ""))
	}, placed)
	r.user = "p1"
	heard := &heardTurns{}
	r.host.AddTurnLearner(heard)
	r.host.AddPromptNote(func(_ context.Context, userID, agentID string) string {
		return "Learned with " + userID + " on " + agentID + ": never more than half a lot."
	})
	r.turn(t, domain.IntentRecommend, "what's EURUSD?")
	r.turn(t, domain.IntentRecommend, "no, I never trade more than half a lot")

	turns := heard.wait(t, 2)
	if turns[0].PrevReply != "" || turns[0].Reply != "EURUSD is 1.0850. Shall I buy 1 lot?" || strings.Join(turns[0].UsedApps, ",") != "int-fx" {
		t.Errorf("first exchange: %+v", turns[0])
	}
	if turns[1].PrevReply != "EURUSD is 1.0850. Shall I buy 1 lot?" || turns[1].UserMessage != "no, I never trade more than half a lot" || turns[1].AgentID != "a1" {
		t.Errorf("second exchange carries the reply it answered: %+v", turns[1])
	}
	if !strings.Contains(r.model.system, "Learned with p1 on a1: never more than half a lot.") {
		t.Errorf("the note reaches the instructions: %q", r.model.system)
	}
}

// A learner is told each ability the reply used, in order, with what kind it is, how it went and what it was given.
func TestTurnLearner_HearsTheStepsInOrderWithWhatEachWasGiven(t *testing.T) {
	r := newRig(func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(domain.ToolCall{ID: "c1", Name: "get_live_quote", Arguments: map[string]any{"pair": "EURUSD"}})
		case 2:
			return calls(domain.ToolCall{ID: "c2", Name: "place_trade", Arguments: map[string]any{"pair": "EURUSD", "side": "buy"}})
		}
		return calls(finishCall("Bought EURUSD.", "done"))
	}, placed)
	r.user = "p1"
	heard := &heardTurns{}
	r.host.AddTurnLearner(heard)
	r.turn(t, domain.IntentExecute, "buy EURUSD")

	steps := heard.wait(t, 1)[0].Steps
	if len(steps) != 2 {
		t.Fatalf("two abilities used: %+v", steps)
	}
	q, tr := steps[0], steps[1]
	if q.Tool != "q" || q.Name != "get_live_quote" || q.Changes || q.State != domain.StepDone || q.Input["pair"] != "EURUSD" || q.App != "int-fx" {
		t.Errorf("the look-up first: %+v", q)
	}
	if tr.Tool != "t" || !tr.Changes || tr.State != domain.StepDone || tr.Input["side"] != "buy" {
		t.Errorf("then the change: %+v", tr)
	}
}
