package runtime_test

import (
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// The loop nudges a model that answers without an action ("You must respond by calling one of the available
// actions…"). The model sees that within the run, but it is never kept in the conversation: kept, it read later as
// something the person had written (found 2026-10-23 in a person's stored chat), and so did the undelivered text that
// made it necessary. Only the person's message and the real reply are kept.
func TestLoopNudgesAreNeverKeptInTheConversation(t *testing.T) {
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{Content: "an undelivered draft"}, nil // no action called: nudged
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": "Here you go."}}}}, nil
	}}
	r := newFollowRig(t, &followRouter{})
	r.hosted.Agent.Skills = nil // no router decisions: the plain loop, where the nudge happens
	r.hosted.Agent.LLMs = []domain.LLM{llm}
	r.say(t, "C1", "What can you do?")

	if llm.n != 2 {
		t.Fatalf("nudged once, then finished: %d calls", llm.n)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != "Here you go." {
		t.Fatalf("the reply is delivered: %q", got)
	}
	var kept []string
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			kept = append(kept, string(m.Role)+": "+m.Content)
			if strings.Contains(m.Content, "You must respond by calling") || m.Content == "an undelivered draft" {
				t.Errorf("kept something only the loop said: %s: %q", m.Role, m.Content)
			}
		}
	}
	userMessages := 0
	for _, k := range kept {
		if strings.HasPrefix(k, string(domain.RoleUser)+": ") {
			userMessages++
		}
	}
	if userMessages != 1 {
		t.Errorf("only the person's own message is kept as theirs: %q", kept)
	}
}

func TestKeptLeavesOutOnlyWhatIsInternal(t *testing.T) {
	in := []domain.Message{{Role: domain.RoleUser, Content: "hi"}, {Role: domain.RoleUser, Content: "nudge", Internal: true}, {Role: domain.RoleAssistant, Content: "hello"}}
	got := domain.Kept(in)
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "hello" || len(in) != 3 {
		t.Fatalf("%v", got)
	}
}
