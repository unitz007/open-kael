package domain_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// stepLog records what a run tells about its steps, as "action:state".
type stepLog struct {
	mu  sync.Mutex
	got []string
}

func (s *stepLog) Step(action string, state domain.StepState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, action+":"+string(state))
}

type answer struct{ yes bool }

func (a answer) RequestApproval(context.Context, domain.ConversationRef, string, int) (bool, error) {
	return a.yes, nil
}

// Each ability the agent uses is told as it starts and as it ends, with how it went; asking the person first is told
// while it waits. Nothing about what went in or came back is passed on.
func TestSteps_EachUseIsToldAsItStartsAndEndsWithHowItWent(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})
	send := &domain.ToolDefinition{ID: "t_send", Name: "send_email", Description: "Sends", IntegrationID: integration.ID, Action: "mock.send", RequiresApproval: true}
	look := &domain.ToolDefinition{ID: "t_look", Name: "find_emails", Description: "Finds", IntegrationID: integration.ID, Action: "mock.find", ReadOnly: true}

	run := func(yes bool) []string {
		sendB, err := domain.HydrateTool(send, nil, integration, registry)
		if err != nil {
			t.Fatal(err)
		}
		lookB, _ := domain.HydrateTool(look, nil, integration, registry)
		broken := &domain.BoundAction{Spec: domain.ActionSpec{Name: "broken"}, Invoke: func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("the service is down")
		}}
		llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
			switch n {
			case 1:
				return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "find_emails", Arguments: map[string]any{}}}}, nil
			case 2:
				return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: "send_email", Arguments: map[string]any{}}}}, nil
			case 3:
				return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "3", Name: "broken", Arguments: map[string]any{}}}}, nil
			}
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "4", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok", "outcome": "cannot"}}}}, nil
		}}
		log := &stepLog{}
		ctx := domain.WithStepObserver(context.Background(), log)
		ctx = domain.WithApprovalRequester(ctx, answer{yes: yes})
		ctx = domain.WithConversation(ctx, domain.ConversationRef{ChatID: "c"})
		if _, _, err := domain.NewNativeLoop([]domain.LLM{llm}, 8).Run(ctx, nil, []*domain.BoundAction{lookB, sendB, broken}); err != nil {
			t.Fatal(err)
		}
		return log.got
	}

	want := func(got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("steps: %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("steps: %v, want %v", got, want)
			}
		}
	}
	want(run(true), []string{
		"find_emails:running", "find_emails:done",
		"send_email:running", "send_email:waiting", "send_email:running", "send_email:done",
		"broken:running", "broken:failed",
	})
	want(run(false), []string{
		"find_emails:running", "find_emails:done",
		"send_email:running", "send_email:waiting", "send_email:declined",
		"broken:running", "broken:failed",
	})
}

func TestSteps_HowAnActionEnded(t *testing.T) {
	cases := []struct {
		out  any
		err  error
		want domain.StepState
	}{
		{map[string]any{"ok": true}, nil, domain.StepDone},
		{"a result", nil, domain.StepDone},
		{nil, errors.New("down"), domain.StepFailed},
		{domain.OutputNotApproved, nil, domain.StepDeclined},
		{domain.OutputAuthPendingPrefix + ": sent a button", nil, domain.StepSignIn},
		{map[string]any{"changed": false, "message": "nothing to move"}, nil, domain.StepFailed},
		{nil, domain.ErrSuperseded, domain.StepStopped},
	}
	for _, c := range cases {
		if got := domain.StepStateOf(c.out, c.err); got != c.want {
			t.Errorf("%v %v: %s, want %s", c.out, c.err, got, c.want)
		}
	}
}
