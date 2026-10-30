package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// twoStepLLM plays a model that finishes with a plain reply (never volunteering choices) and, when asked
// to make buttons from that reply, answers as given.
type twoStepLLM struct {
	mu       sync.Mutex
	reply    string
	derive   func() (*domain.LLMResponse, error)
	deriving []string // what the extra step was shown
	calls    int
}

func (l *twoStepLLM) Call(_ context.Context, msgs []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	for _, a := range actions {
		if a.Name == "offer_choices" {
			var shown []string
			for _, m := range msgs {
				shown = append(shown, m.Content)
			}
			l.deriving = append(l.deriving, strings.Join(shown, "\n---\n"))
			if l.derive == nil {
				return &domain.LLMResponse{}, nil
			}
			return l.derive()
		}
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": l.reply}}}}, nil
}

func offerChoicesCall(choices []any) func() (*domain.LLMResponse, error) {
	return func() (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "o", Name: "offer_choices", Arguments: map[string]any{"choices": choices}}}}, nil
	}
}

func TestDerivedChoices_AnOfferAtTheEndOfAReplyBecomesButtonsAfterTheReplyIsSent(t *testing.T) {
	llm := &twoStepLLM{reply: "Your dentist clashes with the stand-up. Want me to move one of them?", derive: offerChoicesCall(twoChoices())}
	host, hosted, box, store := choiceRig(t, llm, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("what is on tomorrow"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })

	if got := box.texts(); len(got) != 1 || got[0] != llm.reply {
		t.Errorf("the reply goes out as written, with nothing added: %q", got)
	}
	buttons, _ := box.snapshot()
	if len(buttons[0]) != 2 || buttons[0][0].Label != "10:15 tomorrow" {
		t.Errorf("%+v", buttons[0])
	}
	id, _, _ := domain.ParseProposalData(buttons[0][0].Data)
	if p, err := store.GetProposal(context.Background(), id); err != nil || p.PersonID != "u1" || p.Text != llm.reply || len(p.Choices) != 2 {
		t.Errorf("kept like any other choices: %+v %v", p, err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if len(llm.deriving) != 1 || !strings.Contains(llm.deriving[0], "what is on tomorrow") || !strings.Contains(llm.deriving[0], "Want me to move one of them?") {
		t.Errorf("the extra step sees what the person wrote and the reply: %q", llm.deriving)
	}
	if !strings.Contains(llm.deriving[0], "its label starting 'Yes, '") || !strings.Contains(llm.deriving[0], "Never offer 'something else'") {
		t.Errorf("the extra step is told a yes/no question gets Yes and No, and never 'something else': %q", llm.deriving[0])
	}
}

func TestDerivedChoices_ATapOnThemIsHandledAsTyped(t *testing.T) {
	llm := &twoStepLLM{reply: "Clash. Want me to move one?", derive: offerChoicesCall(twoChoices())}
	host, hosted, box, _ := choiceRig(t, llm, true)
	heard := make(chan string, 1)
	host.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) { heard <- m.Text })
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	buttons, _ := box.snapshot()
	host.DispatchInbound(context.Background(), hosted, tap(buttons[0][1].Data))
	select {
	case got := <-heard:
		if got != "Keep my dentist appointment as it is." {
			t.Errorf("%q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the tap never reached the conversation")
	}
}

func TestDerivedChoices_NoExtraStepWhenThereIsNoOfferOrNoWayToShowButtons(t *testing.T) {
	// A plain answer: nothing to offer, and no extra model call.
	plain := &twoStepLLM{reply: "You have one thing on Friday: lunch with Sam at 13:00.", derive: offerChoicesCall(twoChoices())}
	host, hosted, box, _ := choiceRig(t, plain, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("anything on Friday"))
	waitFor(t, "reply", func() bool { return len(box.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if b, _ := box.snapshot(); len(b) != 0 || plain.calls != 1 {
		t.Errorf("an answer with no offer is left alone: buttons=%v calls=%d", b, plain.calls)
	}

	// Choices turned off: an offer stays words, and costs nothing extra.
	off := &twoStepLLM{reply: "Clash. Want me to move one?", derive: offerChoicesCall(twoChoices())}
	host2, hosted2, box2, _ := choiceRig(t, off, false)
	host2.DispatchInbound(context.Background(), hosted2, firstMessage("go"))
	waitFor(t, "reply", func() bool { return len(box2.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if b, _ := box2.snapshot(); len(b) != 0 || off.calls != 1 || box2.texts()[0] != off.reply {
		t.Errorf("without saved options: buttons=%v calls=%d text=%q", b, off.calls, box2.texts())
	}

	// Nobody there to tap: an unattended run never gets buttons.
	un := &twoStepLLM{reply: "Summary. Want me to move one?", derive: offerChoicesCall(twoChoices())}
	host3, _, box3, store3 := choiceRig(t, un, true)
	if err := host3.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "check my calendar"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reply", func() bool { return len(box3.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if b, _ := box3.snapshot(); len(b) != 0 || un.calls != 1 || len(store3.m) != 0 {
		t.Errorf("unattended: buttons=%v calls=%d kept=%d", b, un.calls, len(store3.m))
	}
}

func TestDerivedChoices_AFailureOrAnEmptyAnswerLeavesTheReplyAsItWas(t *testing.T) {
	for name, derive := range map[string]func() (*domain.LLMResponse, error){
		"model fails":      func() (*domain.LLMResponse, error) { return nil, errors.New("boom") },
		"no choices":       func() (*domain.LLMResponse, error) { return &domain.LLMResponse{Content: "none"}, nil },
		"one is not a set": offerChoicesCall([]any{map[string]any{"label": "Only", "say": "Only one."}}),
		"malformed":        offerChoicesCall([]any{"x", 3}),
	} {
		llm := &twoStepLLM{reply: "Clash. Want me to move one?", derive: derive}
		host, hosted, box, store := choiceRig(t, llm, true)
		host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
		waitFor(t, name, func() bool { return len(box.texts()) == 1 })
		time.Sleep(100 * time.Millisecond)
		if b, _ := box.snapshot(); len(b) != 0 || len(store.m) != 0 || box.texts()[0] != llm.reply {
			t.Errorf("%s: buttons=%v kept=%d text=%q", name, b, len(store.m), box.texts())
		}
	}
}

func TestDerivedChoices_ChoicesTheAgentGaveAreNotReplaced(t *testing.T) {
	llm := finishWithChoices("Clash. Want me to move one?", twoChoices(), "ok")
	host, hosted, box, _ := choiceRig(t, llm, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	time.Sleep(100 * time.Millisecond)
	if llm.n != 1 {
		t.Errorf("the agent's own choices are used, with no extra model call: %d", llm.n)
	}
}

// streamingBox is a messenger that sends replies by streaming, as Telegram does.
type streamingBox struct{ *buttonOutbox }

func (s streamingBox) StreamReply(_ context.Context, _ *domain.Identity, _ string, _ string, _ string, chunks <-chan string) (string, string, error) {
	var b strings.Builder
	for c := range chunks {
		b.WriteString(c)
	}
	s.buttonOutbox.mu2.Lock()
	s.buttonOutbox.onMsg = append(s.buttonOutbox.onMsg, "streamed:"+b.String())
	s.buttonOutbox.mu2.Unlock()
	return "m9", "", nil
}

func TestDerivedChoices_AlsoWhenTheReplyIsStreamed(t *testing.T) {
	llm := &twoStepLLM{reply: "Clash. Want me to move one?", derive: offerChoicesCall(twoChoices())}
	box := &buttonOutbox{}
	execs := domain.NewExecutorRegistry()
	execs.Register("slack", streamingBox{box})
	deps := slackDeps(func(d *runtime.AgentDeps) { d.Executors = execs; d.Memory = newMemMemory() })
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5}, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	store := newMemProposals()
	host.SetProposalStore(store)
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	id, _, _ := domain.ParseProposalData(func() string { b, _ := box.snapshot(); return b[0][0].Data }())
	if p, err := store.GetProposal(context.Background(), id); err != nil || p.MessageID != "m9" {
		t.Errorf("the buttons go on the message that was streamed: %+v %v", p, err)
	}
}

func TestDerivedChoices_AFailedFirstModelFallsThroughToTheNext(t *testing.T) {
	bad := &twoStepLLM{reply: "Clash. Want me to move one?", derive: func() (*domain.LLMResponse, error) { return nil, errors.New("down") }}
	good := &twoStepLLM{reply: "unused", derive: offerChoicesCall(twoChoices())}
	// the first model answers the turn and then fails on the extra step; the second makes the buttons
	host, hosted, box, _ := choiceRig(t, bad, true)
	hosted.Agent.LLMs = []domain.LLM{bad, good}
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
}

func TestDerivedChoices_OnlyTheEndOfALongReplyDecidesWhetherItOffersAnything(t *testing.T) {
	long := "Which is better, 10:15 or 11:00? " + strings.Repeat("Both are free and your day is otherwise quiet. ", 12) + "That is everything on for tomorrow."
	llm := &twoStepLLM{reply: long, derive: offerChoicesCall(twoChoices())}
	host, hosted, box, _ := choiceRig(t, llm, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "reply", func() bool { return len(box.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if b, _ := box.snapshot(); len(b) != 0 || llm.calls != 1 {
		t.Errorf("a question long ago in the reply is not an offer at its end: buttons=%v calls=%d", b, llm.calls)
	}
}

func TestDerivedChoices_OnlyTheOfferedActionCounts(t *testing.T) {
	wrong := func() (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "x", Name: "something_else", Arguments: map[string]any{"choices": twoChoices()}}}}, nil
	}
	llm := &twoStepLLM{reply: "Clash. Want me to move one?", derive: wrong}
	host, hosted, box, store := choiceRig(t, llm, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "reply", func() bool { return len(box.texts()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if b, _ := box.snapshot(); len(b) != 0 || len(store.m) != 0 {
		t.Errorf("a call to anything else is not a set of choices: %v", b)
	}
}

// Whatever model a creator gives their agent, the buttons are made by the platform's own.
func TestDerivedChoices_ThePlatformModelMakesThemNotTheAgentsOwn(t *testing.T) {
	// the agent's own model would make useless buttons if it were asked
	agentLLM := &twoStepLLM{reply: "Clash. Want me to move one?", derive: offerChoicesCall([]any{
		map[string]any{"label": "Yes", "say": "Yes."}, map[string]any{"label": "No", "say": "No."}})}
	platform := &twoStepLLM{derive: offerChoicesCall(twoChoices())}
	host, hosted, box, _ := choiceRig(t, agentLLM, true)
	host.SetChoiceModel(platform)
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	buttons, _ := box.snapshot()
	if buttons[0][0].Label != "10:15 tomorrow" {
		t.Errorf("made by the platform's model: %+v", buttons[0])
	}
	if len(agentLLM.deriving) != 0 || agentLLM.calls != 1 {
		t.Errorf("the agent's own model only answers the turn: derive=%d calls=%d", len(agentLLM.deriving), agentLLM.calls)
	}
	if len(platform.deriving) != 1 {
		t.Errorf("the platform model was asked once: %d", len(platform.deriving))
	}
}
