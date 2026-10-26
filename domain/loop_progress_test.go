package domain_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// specLLM answers by turn and remembers which actions it was offered each time.
type specLLM struct {
	n       int
	offered [][]string
	call    func(n int) *domain.LLMResponse
}

func (s *specLLM) Call(_ context.Context, _ []domain.Message, specs []domain.ActionSpec) (*domain.LLMResponse, error) {
	s.n++
	var names []string
	for _, sp := range specs {
		names = append(names, sp.Name)
	}
	s.offered = append(s.offered, names)
	return s.call(s.n), nil
}

func finishAction() *domain.BoundAction {
	return &domain.BoundAction{Spec: domain.ActionSpec{Name: domain.FinishActionName, InputSchema: domain.Schema{Type: domain.SchemaTypeObject,
		Properties: map[string]domain.Schema{"content": {Type: domain.SchemaTypeString}}}}}
}

func calls(n int, name string, args ...map[string]any) *domain.LLMResponse {
	var out []domain.ToolCall
	for i, a := range args {
		out = append(out, domain.ToolCall{ID: fmt.Sprintf("%d-%d", n, i), Name: name, Arguments: a})
	}
	return &domain.LLMResponse{ToolCalls: out}
}

func finish(text string) *domain.LLMResponse {
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": text}}}}
}

// The same look-up twice in one reply is the same question: it gets the answer it got before, the service is asked
// once, and nothing counts against the reply. A look-up has no per-reply limit of its own (comparing four players is
// four look-ups), whatever its MaxCalls says; that stays for changes.
func TestLoop_ALookUpAskedAgainGetsItsEarlierAnswer(t *testing.T) {
	asked := map[string]int{}
	stats := &domain.BoundAction{Spec: domain.ActionSpec{Name: "player_stats", ReadOnly: true, MaxCalls: 1},
		Invoke: func(_ context.Context, in map[string]any) (any, error) {
			asked[fmt.Sprint(in["name"])]++
			return "stats for " + fmt.Sprint(in["name"]), nil
		}}
	llm := &specLLM{call: func(n int) *domain.LLMResponse {
		switch n {
		case 1:
			return calls(n, "player_stats", map[string]any{"name": "Saka"}, map[string]any{"name": "Kluivert"}, map[string]any{"name": "Semenyo"}, map[string]any{"name": "Eze"})
		case 2, 3:
			return calls(n, "player_stats", map[string]any{"name": "Saka"})
		}
		return finish("Kluivert is the pick.")
	}}
	result, transcript, err := domain.NewNativeLoop([]domain.LLM{llm}, 10).Run(context.Background(), nil, []*domain.BoundAction{stats, finishAction()})
	if err != nil {
		t.Fatal(err)
	}
	if asked["Saka"] != 1 || len(asked) != 4 {
		t.Errorf("the service was asked %v; want each player once", asked)
	}
	var again int
	for _, m := range transcript {
		if m.Role == domain.RoleTool && strings.Contains(m.Content, "the same as before") && strings.Contains(m.Content, "stats for Saka") {
			again++
		}
	}
	if again == 0 {
		t.Error("asked again, it was given the earlier answer")
	}
	// Repeating itself is not progress: after two such steps the reply ends with an answer from what it found.
	if result.Status != domain.LoopStatusComplete || result.Content != "Kluivert is the pick." {
		t.Fatalf("got %s %q", result.Status, result.Content)
	}
}

// A reply that can't go on ends with an answer from what it found, never an error: here every call fails. The last
// call offers only finish.
func TestLoop_ARepliesThatCantGoOnEndsWithAnAnswer(t *testing.T) {
	broken := &domain.BoundAction{Spec: domain.ActionSpec{Name: "fixtures", ReadOnly: true},
		Invoke: func(context.Context, map[string]any) (any, error) { return nil, fmt.Errorf("service down") }}
	llm := &specLLM{call: func(n int) *domain.LLMResponse {
		if n <= 3 {
			return calls(n, "fixtures", map[string]any{"week": n})
		}
		return finish("I couldn't get the fixtures just now; ask me again in a bit.")
	}}
	result, _, err := domain.NewNativeLoop([]domain.LLM{llm}, 10).Run(context.Background(), nil, []*domain.BoundAction{broken, finishAction()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.LoopStatusComplete || !strings.Contains(result.Content, "couldn't get the fixtures") {
		t.Fatalf("got %s %q", result.Status, result.Content)
	}
	if last := llm.offered[len(llm.offered)-1]; len(last) != 1 || last[0] != domain.FinishActionName {
		t.Errorf("the last call offers only finish: %v", last)
	}
}

// Out of steps, it still answers with what it has.
func TestLoop_OutOfStepsItStillAnswers(t *testing.T) {
	step := &domain.BoundAction{Spec: domain.ActionSpec{Name: "step", ReadOnly: true},
		Invoke: func(context.Context, map[string]any) (any, error) { return "ok", nil }}
	llm := &specLLM{call: func(n int) *domain.LLMResponse {
		if n <= 3 {
			return calls(n, "step", map[string]any{"i": n})
		}
		return finish("Here's what I got through.")
	}}
	result, _, err := domain.NewNativeLoop([]domain.LLM{llm}, 3).Run(context.Background(), nil, []*domain.BoundAction{step, finishAction()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != domain.LoopStatusComplete || result.Content != "Here's what I got through." {
		t.Fatalf("got %s %q", result.Status, result.Content)
	}
}
