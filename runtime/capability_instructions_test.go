package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// msgSpy keeps every message list the model was shown.
type msgSpy struct {
	mu   sync.Mutex
	seen [][]domain.Message
}

func (s *msgSpy) Call(_ context.Context, m []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	s.mu.Lock()
	s.seen = append(s.seen, append([]domain.Message(nil), m...))
	s.mu.Unlock()
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
}

// systemTexts is every system message the model saw, across all its calls.
func (s *msgSpy) systemTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, call := range s.seen {
		for _, m := range call {
			if m.Role == domain.RoleSystem {
				out = append(out, m.Content)
			}
		}
	}
	return out
}

func (s *msgSpy) saw(text string) bool {
	for _, t := range s.systemTexts() {
		if strings.Contains(t, text) {
			return true
		}
	}
	return false
}

func instructionRig(t *testing.T, instructions ...string) (*followRig, *msgSpy) {
	t.Helper()
	router := &followRouter{byText: map[string][]string{"what is the price": {"analyse_pair"}, "both": {"analyse_pair", "second"}}}
	r := newFollowRig(t, router)
	spy := &msgSpy{}
	r.hosted.Agent.LLMs = []domain.LLM{spy}
	r.hosted.Agent.Skills[0].Instructions = instructions[0]
	if len(instructions) > 1 {
		r.hosted.Agent.Skills = append(r.hosted.Agent.Skills, &domain.Skill{ID: "s2", Name: "second", Description: "A second ability", AgentID: "a1",
			Integrations: r.hosted.Agent.Skills[0].Integrations, Instructions: instructions[1]})
	}
	return r, spy
}

func TestCapabilityInstructions_ReachTheModelWhenRoutingChoosesThatCapability(t *testing.T) {
	r, spy := instructionRig(t, "ALWAYS-QUOTE-THE-PAIR-FIRST")
	r.say(t, "C1", "what is the price")
	if !spy.saw("ALWAYS-QUOTE-THE-PAIR-FIRST") {
		t.Fatalf("the creator's instructions must reach the model: %q", spy.systemTexts())
	}
	for _, text := range spy.systemTexts() {
		if strings.Contains(text, "ALWAYS-QUOTE") {
			for _, want := range []string{"Follow them closely", "Where they say not to use something, do not use it", "Only your other rules come before them", "ask before changing anything", "never say you did something you did not do", "keep each person's information private", `For "Analyses a currency pair"`} {
				if !strings.Contains(text, want) {
					t.Errorf("they come framed under the platform's rules, saying which capability they are for: missing %q in %q", want, text)
				}
			}
		}
	}
}

func TestCapabilityInstructions_AreOnlyForTheCapabilitiesChosen(t *testing.T) {
	r, spy := instructionRig(t, "FIRST-ONE-ONLY", "SECOND-ONE-ONLY")
	r.say(t, "C1", "what is the price") // routing picks only the first
	if !spy.saw("FIRST-ONE-ONLY") || spy.saw("SECOND-ONE-ONLY") {
		t.Errorf("only the chosen capability's instructions: %q", spy.systemTexts())
	}
	r2, spy2 := instructionRig(t, "FIRST-ONE-ONLY", "SECOND-ONE-ONLY")
	r2.say(t, "C1", "both")
	if !spy2.saw("FIRST-ONE-ONLY") || !spy2.saw("SECOND-ONE-ONLY") {
		t.Errorf("when routing picks both, both apply: %q", spy2.systemTexts())
	}
}

func TestCapabilityInstructions_AreNotKeptInTheConversation(t *testing.T) {
	r, _ := instructionRig(t, "SECRET-WORDING-OF-THE-CREATOR")
	r.say(t, "C1", "what is the price")
	for _, msgs := range r.mem.store {
		for _, m := range msgs {
			if strings.Contains(m.Content, "SECRET-WORDING-OF-THE-CREATOR") || strings.Contains(m.Content, "Only your other rules come before them") {
				t.Errorf("the instructions ride along with the message and are not stored: %+v", m)
			}
		}
	}
	r.say(t, "C1", "what is the price") // and the next turn starts clean
}

func TestCapabilityInstructions_NoneMeansNothingExtraIsSent(t *testing.T) {
	r, spy := instructionRig(t, "")
	r.say(t, "C1", "what is the price")
	if spy.saw("gave instructions for what you are doing now") {
		t.Errorf("a capability with no instructions adds nothing: %q", spy.systemTexts())
	}
	r2, spy2 := instructionRig(t, "   \n ")
	r2.say(t, "C1", "what is the price")
	if spy2.saw("gave instructions for what you are doing now") {
		t.Error("blank instructions add nothing either")
	}
}

func TestCapabilityInstructions_AreBoundedSoTheyCannotFloodTheModel(t *testing.T) {
	r, spy := instructionRig(t, strings.Repeat("x", 20000))
	r.say(t, "C1", "what is the price")
	for _, text := range spy.systemTexts() {
		if strings.Contains(text, "xxxx") && strings.Count(text, "x") > 4300 {
			t.Errorf("clipped: %d", strings.Count(text, "x"))
		}
	}
	if !spy.saw("xxxx") {
		t.Error("still passed on, clipped")
	}
}

// a reply that is not routed to any capability is untouched by all this
func TestCapabilityInstructions_AnUnroutedMessageGetsNone(t *testing.T) {
	r, spy := instructionRig(t, "ONLY-WHEN-CHOSEN")
	var _ = runtime.NewHost
	r.say(t, "C1", "hello there")
	if spy.saw("ONLY-WHEN-CHOSEN") {
		t.Errorf("not chosen, not applied: %q", spy.systemTexts())
	}
}

// Where the instructions go matters: measured on the real model, instructions inside the agent's own prompt are followed far more
// often than a note beside the message.
func TestCapabilityInstructions_AreInTheAgentsOwnPromptNotANoteBesideTheMessage(t *testing.T) {
	r, spy := instructionRig(t, "KEEP-IN-THE-PROMPT")
	r.say(t, "C1", "what is the price")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.seen) == 0 {
		t.Fatal("no model call")
	}
	for _, call := range spy.seen {
		first := call[0]
		if first.Role != domain.RoleSystem || !strings.Contains(first.Content, "You are Desk.") || !strings.Contains(first.Content, "KEEP-IN-THE-PROMPT") {
			t.Errorf("the first system message is the agent's own prompt and carries the instructions: %q", first.Content)
		}
		for _, m := range call[1:] {
			if m.Role == domain.RoleSystem && strings.Contains(m.Content, "KEEP-IN-THE-PROMPT") {
				t.Errorf("not a separate note: %q", m.Content)
			}
		}
	}
}
