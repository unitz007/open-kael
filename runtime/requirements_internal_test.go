package runtime

import (
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestLastAssistantText(t *testing.T) {
	msgs := []domain.Message{
		{Role: domain.RoleSystem, Content: "be helpful"},
		{Role: domain.RoleUser, Content: "buy EURUSD"},
		{Role: domain.RoleAssistant, Content: "  Shall I place it?  "},
		{Role: domain.RoleAssistant, Content: "", ToolCalls: []domain.ToolCall{{ID: "1"}}}, // no words: skipped
		{Role: domain.RoleTool, Content: "data"},
		{Role: domain.RoleUser, Content: "yes"},
	}
	if got := lastAssistantText(msgs); got != "Shall I place it?" {
		t.Errorf("want the last assistant message that has words, trimmed: %q", got)
	}
	// How an agent really speaks: by calling finish. The words are in the call.
	spoken := []domain.Message{
		{Role: domain.RoleUser, Content: "buy EURUSD"},
		{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": "Shall I place it?", "outcome": "need_details"}}}},
		{Role: domain.RoleTool, Name: domain.FinishActionName, Content: "done"},
		{Role: domain.RoleUser, Content: "yes"},
	}
	if got := lastAssistantText(spoken); got != "Shall I place it?" {
		t.Errorf("the words of a finish call are the assistant's message: %q", got)
	}
	// A call that isn't finish is not speech.
	other := []domain.Message{{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "x", Name: "place_trade", Arguments: map[string]any{"content": "not words"}}}}}
	if got := lastAssistantText(other); got != "" {
		t.Errorf("only finish counts as speech: %q", got)
	}
	if lastAssistantText(nil) != "" || lastAssistantText([]domain.Message{{Role: domain.RoleUser, Content: "hi"}}) != "" {
		t.Error("no assistant message means nothing earlier")
	}
	long := strings.Repeat("é", maxEarlierRunes+50)
	got := lastAssistantText([]domain.Message{{Role: domain.RoleAssistant, Content: long}})
	if r := []rune(got); len(r) != maxEarlierRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long message is cut at %d characters (not bytes): %d", maxEarlierRunes, len(r))
	}
}
