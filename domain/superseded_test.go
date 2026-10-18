package domain_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func supersedingAsk() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{Name: "ask", Description: "asks"},
		Invoke: func(context.Context, map[string]any) (any, error) {
			return nil, domain.ErrSuperseded // the person wrote a new message while it waited
		},
	}
}

// Nothing has been changed yet: the turn ends quietly (complete, no content, so nothing is
// sent), the history keeps the question and that it went unanswered, and no further model call
// is made. The person's new message is handled next.
func TestLoop_ANewMessageWhileWaitingEndsTheTurnQuietly(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "ask", Arguments: map[string]any{}},
			{ID: "2", Name: "other", Arguments: map[string]any{}}, // asked in the same breath
		}}, nil
	}}
	other := &domain.BoundAction{Spec: domain.ActionSpec{Name: "other"}, Invoke: func(context.Context, map[string]any) (any, error) {
		t.Error("a call after the cut-short question is not run")
		return "x", nil
	}}
	res, msgs, err := domain.NewNativeLoop([]domain.LLM{llm}, 5).Run(context.Background(), nil, []*domain.BoundAction{supersedingAsk(), other})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.LoopStatusComplete || res.Content != "" {
		t.Errorf("complete and silent: %+v", res)
	}
	if llm.n != 1 {
		t.Errorf("the model is not asked again: %d calls", llm.n)
	}
	var answered = map[string]string{}
	for _, m := range msgs {
		if m.Role == domain.RoleTool {
			answered[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(answered["1"], "not answered") || !strings.Contains(answered["2"], "not run") {
		t.Errorf("every call in the history has an answer, so the history stays valid: %v", answered)
	}
}

// Something was already changed this turn: ending quietly would leave the person not knowing, so
// the turn carries on and the model reports what happened.
func TestLoop_ANewMessageAfterAChangeDoesNotSilenceTheReport(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		switch n {
		case 1:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "change", Arguments: map[string]any{}}}}, nil
		case 2:
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: "ask", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "3", Name: domain.FinishActionName, Arguments: map[string]any{"content": "Changed it; you wrote something new, so I stopped asking."}}}}, nil
	}}
	change := &domain.BoundAction{Spec: domain.ActionSpec{Name: "change", Changes: true}, Invoke: func(context.Context, map[string]any) (any, error) {
		return map[string]any{"changed": true, "message": "ok"}, nil
	}}
	res, _, err := domain.NewNativeLoop([]domain.LLM{llm}, 6).Run(context.Background(), nil, []*domain.BoundAction{change, supersedingAsk()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == "" || llm.n != 3 {
		t.Errorf("the turn finishes with a report after a change: %+v after %d calls", res, llm.n)
	}
}
