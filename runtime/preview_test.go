package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// previewLLM answers with the calls it is given and keeps what it was shown.
type previewLLM struct {
	calls   []domain.ToolCall
	err     error
	msgs    []domain.Message
	actions []domain.ActionSpec
}

func (r *previewLLM) Call(_ context.Context, m []domain.Message, a []domain.ActionSpec) (*domain.LLMResponse, error) {
	r.msgs, r.actions = m, a
	return &domain.LLMResponse{ToolCalls: r.calls}, r.err
}

func defs() []*domain.ToolDefinition {
	return []*domain.ToolDefinition{
		{ID: "t1", Name: "check_trades", FunctionName: "check_trades", Description: "Shows what is open.", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}},
		{ID: "t2", Name: "Place Trade", FunctionName: "place_trade", Description: "Places a trade.", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}},
		{ID: "t3", Name: "off", FunctionName: "off", Description: "Switched off.", Disabled: true, ReadOnly: true},
	}
}

func previewAgent() (*domain.Agent, *domain.Skill) {
	skill := &domain.Skill{Name: "trades", Description: "Looks after my trades", Instructions: "Always check what is open before anything else."}
	return &domain.Agent{Name: "Nova", Description: "Trading helper", Instructions: "You are calm.", Skills: []*domain.Skill{skill}}, skill
}

func aCall(name string) domain.ToolCall {
	return domain.ToolCall{ID: name, Name: name, Arguments: map[string]any{}}
}

func TestPreviewToolUse_TriesTheMessageTheWayARealTurnWouldShowIt(t *testing.T) {
	agent, skill := previewAgent()
	llm := &previewLLM{calls: []domain.ToolCall{aCall("check_trades")}}
	if _, err := runtime.PreviewToolUse(context.Background(), llm, agent, skill, defs(), "what is open?"); err != nil {
		t.Fatal(err)
	}
	if len(llm.msgs) != 3 || llm.msgs[0].Role != domain.RoleSystem || llm.msgs[1].Role != domain.RoleSystem || llm.msgs[2].Role != domain.RoleUser || llm.msgs[2].Content != "what is open?" {
		t.Fatalf("the agent's own prompt, the creator's instructions, then the message: %+v", llm.msgs)
	}
	for _, want := range []string{"You are Nova.", "Trading helper", "You are calm."} {
		if !strings.Contains(llm.msgs[0].Content, want) {
			t.Errorf("the agent's own prompt must carry %q: %q", want, llm.msgs[0].Content)
		}
	}
	if !strings.Contains(llm.msgs[1].Content, "Always check what is open before anything else.") || !strings.Contains(llm.msgs[1].Content, "never against your other rules") {
		t.Errorf("the capability's instructions, framed as in a real turn: %q", llm.msgs[1].Content)
	}
	var names []string
	for _, a := range llm.actions {
		names = append(names, a.Name)
	}
	if got := strings.Join(names, ","); got != "check_trades,place_trade,"+domain.FinishActionName {
		t.Errorf("the abilities that are on, by the names the model uses, and the way to finish: %s", got)
	}
	if !llm.actions[0].ReadOnly || llm.actions[1].ReadOnly || !llm.actions[1].Changes {
		t.Errorf("each marked as the real thing is: %+v", llm.actions)
	}
}

func TestPreviewToolUse_NoInstructionsMeansNoExtraMessage(t *testing.T) {
	agent, skill := previewAgent()
	skill.Instructions = "  "
	llm := &previewLLM{}
	if _, err := runtime.PreviewToolUse(context.Background(), llm, agent, skill, defs(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(llm.msgs) != 2 {
		t.Errorf("only the agent's prompt and the message: %+v", llm.msgs)
	}
}

func TestPreviewToolUse_ReportsWhatItWouldReachForInPlainWords(t *testing.T) {
	agent, skill := previewAgent()
	run := func(calls ...domain.ToolCall) runtime.ToolUse {
		got, err := runtime.PreviewToolUse(context.Background(), &previewLLM{calls: calls}, agent, skill, defs(), "x")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := run(aCall("check_trades")); strings.Join(got.Uses, ",") != "Check trades" || got.Changes {
		t.Errorf("a look-up, named for people, that changes nothing: %+v", got)
	}
	if got := run(aCall("check_trades"), aCall("place_trade"), aCall("check_trades")); strings.Join(got.Uses, ",") != "Check trades,Place Trade" || !got.Changes {
		t.Errorf("in order, once each, and the change is flagged: %+v", got)
	}
	if got := run(aCall(domain.FinishActionName)); len(got.Uses) != 0 || got.Changes {
		t.Errorf("answering without any ability uses none: %+v", got)
	}
	if got := run(aCall("not_offered"), aCall("off")); len(got.Uses) != 0 {
		t.Errorf("something that was not offered, or is switched off, is not counted: %+v", got)
	}
}

func TestPreviewToolUse_AFailureIsReported(t *testing.T) {
	agent, skill := previewAgent()
	if _, err := runtime.PreviewToolUse(context.Background(), &previewLLM{err: errors.New("down")}, agent, skill, defs(), "x"); err == nil {
		t.Error("the model failing is an error, not 'it would use nothing'")
	}
}
