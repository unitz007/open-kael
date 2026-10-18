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

// buttonMessenger shows choices: it records the buttons and the final text of the message,
// and hands the test the token so it can play the person tapping.
type buttonMessenger struct {
	fakeInteractiveMessenger
	mu      sync.Mutex
	buttons []string
	text    string
	updated string
	tokens  chan string
}

func (b *buttonMessenger) PostChoicePrompt(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, text string, options []string, token string) (string, error) {
	b.mu.Lock()
	b.buttons, b.text = options, text
	b.mu.Unlock()
	b.tokens <- token
	return "msg-1", nil
}
func (b *buttonMessenger) UpdateMessage(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, _ string, text string) error {
	b.mu.Lock()
	b.updated = text
	b.mu.Unlock()
	return nil
}

func TestChoiceRequester_ReturnsTheTappedOptionAndShowsTheOutcome(t *testing.T) {
	m := &buttonMessenger{tokens: make(chan string, 1)}
	r := runtime.NewMessengerChoiceRequester(m)
	conv := domain.ConversationRef{Provider: "telegram", ChatID: "C1"}
	got := make(chan int, 1)
	go func() {
		a, err := r.RequestChoice(context.Background(), nil, "", conv, "Which size?", []string{"10,000", "50,000"}, 5)
		if err != nil {
			t.Error(err)
		}
		got <- a.Index
	}()
	token := <-m.tokens
	if !r.Resolve(token, 1) {
		t.Fatal("a pending question resolves")
	}
	if i := <-got; i != 1 {
		t.Errorf("the second option was tapped: %d", i)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Join(m.buttons, "|") != "10,000|50,000|"+domain.ChoiceOtherLabel {
		t.Errorf("one button per option, then 'something else': %q", m.buttons)
	}
	if !strings.Contains(m.updated, "Which size?") || !strings.Contains(m.updated, "50,000") {
		t.Errorf("the message shows what was chosen: %q", m.updated)
	}
	if r.Resolve(token, 0) {
		t.Error("a question can only be answered once")
	}
}

func TestChoiceRequester_SomethingElseIsThePositionAfterTheOptions(t *testing.T) {
	m := &buttonMessenger{tokens: make(chan string, 1)}
	r := runtime.NewMessengerChoiceRequester(m)
	got := make(chan int, 1)
	go func() {
		a, _ := r.RequestChoice(context.Background(), nil, "", domain.ConversationRef{}, "Which?", []string{"a", "b"}, 5)
		got <- a.Index
	}()
	r.Resolve(<-m.tokens, 2)
	if i := <-got; i != 2 {
		t.Errorf("'something else' is len(options): %d", i)
	}
}

func TestChoiceRequester_NoAnswerIsATimeoutNeverAGuess(t *testing.T) {
	m := &buttonMessenger{tokens: make(chan string, 1)}
	r := runtime.NewMessengerChoiceRequester(m)
	start := time.Now()
	_, err := r.RequestChoice(context.Background(), nil, "", domain.ConversationRef{}, "Which?", []string{"a", "b"}, 1)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("a question nobody answers times out with an error: %v", err)
	}
	token := <-m.tokens
	if r.Resolve(token, 0) {
		t.Error("a timed-out question can't be answered later")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !strings.Contains(m.updated, "No answer") {
		t.Errorf("the message says it went unanswered: %q", m.updated)
	}
}

func TestChoiceRequester_AMessengerWithoutButtonsIsAnErrorNotASilentSkip(t *testing.T) {
	r := runtime.NewMessengerChoiceRequester(&fakeInteractiveMessenger{posted: make(chan string, 1)})
	if _, err := r.RequestChoice(context.Background(), nil, "", domain.ConversationRef{}, "Which?", []string{"a", "b"}, 1); err == nil {
		t.Fatal("approval-only messengers can't ask choices")
	}
}

// ── the agent asks, through a real turn ──────────────────────────────────────

// askingLLM first asks a question, then finishes by repeating the tool's result.
type askingLLM struct {
	mu     sync.Mutex
	args   map[string]any
	calls  int
	offers [][]string
}

func (l *askingLLM) Call(_ context.Context, msgs []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	var names []string
	for _, a := range actions {
		names = append(names, a.Name)
	}
	l.offers = append(l.offers, names)
	if l.calls == 1 {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: runtime.AskOptionsActionName, Arguments: l.args}}}, nil
	}
	last := msgs[len(msgs)-1]
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "result: " + last.Content}}}}, nil
}

// choosingExecutor is the messenger: it records replies and answers questions with a fixed tap.
type choosingExecutor struct {
	outbox
	tap       int
	err       error
	asked     []string
	askedOpts [][]string
	timeouts  []int
}

func (c *choosingExecutor) RequestChoice(_ context.Context, _ domain.ConversationRef, q string, opts []string, timeoutSeconds int) (domain.ChoiceAnswer, error) {
	c.timeouts = append(c.timeouts, timeoutSeconds)
	c.asked = append(c.asked, q)
	c.askedOpts = append(c.askedOpts, opts)
	return domain.ChoiceAnswer{Index: c.tap}, c.err
}

func choiceTurn(t *testing.T, ex *choosingExecutor, llm *askingLLM) []string {
	t.Helper()
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", ex)
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })}
	host := runtime.NewHost()
	host.Register(hosted)
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	if _, err := host.HandleTurn(context.Background(), hosted, conv, "help me pick"); err != nil {
		t.Fatal(err)
	}
	return ex.texts()
}

func TestAskOptions_TheAgentAsksAndGetsTheOptionTheyChose(t *testing.T) {
	ex := &choosingExecutor{tap: 1}
	llm := &askingLLM{args: map[string]any{"question": "Which size?", "options": []any{"10,000", " 50,000 ", "10,000", ""}}}
	got := choiceTurn(t, ex, llm)

	if len(ex.asked) != 1 || ex.asked[0] != "Which size?" || strings.Join(ex.askedOpts[0], "|") != "10,000|50,000" {
		t.Fatalf("the person is asked the question with tidy, distinct options: %v %v", ex.asked, ex.askedOpts)
	}
	if len(got) != 1 || !strings.Contains(got[0], "50,000") || !strings.Contains(got[0], "answered") || strings.Contains(got[0], "10,000") {
		t.Errorf("the model sees the option they chose: %q", got)
	}
	if len(ex.timeouts) != 1 || ex.timeouts[0] != 120 {
		t.Errorf("a question waits two minutes, not the ten an approval does: %v", ex.timeouts)
	}
	if !strings.Contains(strings.Join(llm.offers[0], ","), runtime.AskOptionsActionName) {
		t.Errorf("the action is offered where the messenger can ask: %v", llm.offers[0])
	}
}

func TestAskOptions_NotOfferedWhereTheMessengerCannotAsk(t *testing.T) {
	llm := &askingLLM{args: map[string]any{"question": "Q?", "options": []any{"a", "b"}}}
	agent := &domain.Agent{ID: "a1", LLMs: []domain.LLM{llm}, MaxIterations: 3}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &outbox{}) // a messenger with no buttons
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })}
	host := runtime.NewHost()
	host.Register(hosted)
	host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}, "hi")
	for _, offered := range llm.offers {
		if strings.Contains(strings.Join(offered, ","), runtime.AskOptionsActionName) {
			t.Fatalf("no buttons, no question action: %v", offered)
		}
	}
}

func TestAskOptions_SomethingElseAndNoAnswerAreToldToTheModelPlainly(t *testing.T) {
	for name, ex := range map[string]*choosingExecutor{
		"something else": {tap: 2},
		"no answer":      {err: context.DeadlineExceeded},
	} {
		llm := &askingLLM{args: map[string]any{"question": "Which?", "options": []any{"a", "b"}}}
		got := choiceTurn(t, ex, llm)
		want := map[string]string{"something else": "Something else", "no answer": "did not answer"}[name]
		if len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("%s: the model must be told %q: %q", name, want, got)
		}
	}
}

func TestAskOptions_BadQuestionsGoBackToTheModelAsErrors(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"one option":  {"question": "Q?", "options": []any{"a"}},
		"seven":       {"question": "Q?", "options": []any{"a", "b", "c", "d", "e", "f", "g"}},
		"no question": {"question": " ", "options": []any{"a", "b"}},
		"long label":  {"question": "Q?", "options": []any{"a", strings.Repeat("x", 60)}},
		"duplicates":  {"question": "Q?", "options": []any{"a", "A"}},
	} {
		ex := &choosingExecutor{}
		got := choiceTurn(t, ex, &askingLLM{args: args})
		if len(ex.asked) != 0 {
			t.Errorf("%s: nobody is asked a bad question: %v", name, ex.asked)
		}
		if len(got) != 1 || !strings.Contains(got[0], "ask_options:") {
			t.Errorf("%s: the model is told what was wrong: %q", name, got)
		}
	}
}

// Typed words answer the question waiting in THEIR conversation only, and a command never does.
func TestChoiceRequester_TypedWordsAnswerOnlyTheirOwnConversationsQuestion(t *testing.T) {
	m := &buttonMessenger{tokens: make(chan string, 1)}
	r := runtime.NewMessengerChoiceRequester(m)
	conv := domain.ConversationRef{Provider: "telegram", IdentityID: "bot1", ChatID: "C1", SenderID: "U1"}
	got := make(chan domain.ChoiceAnswer, 1)
	go func() {
		a, _ := r.RequestChoice(context.Background(), nil, "", conv, "Which?", []string{"a", "b"}, 5)
		got <- a
	}()
	<-m.tokens

	other := conv
	other.ChatID = "C2"
	if r.AnswerWithText(other, "b please") {
		t.Error("another chat's words are not the answer")
	}
	otherPerson := conv
	otherPerson.SenderID = "U2"
	if r.AnswerWithText(otherPerson, "b please") {
		t.Error("another person's words are not the answer")
	}
	for _, notAnAnswer := range []string{"/start", "   ", ""} {
		if r.AnswerWithText(conv, notAnAnswer) {
			t.Errorf("%q is not an answer", notAnAnswer)
		}
	}
	if !r.AnswerWithText(conv, "  the b one  ") {
		t.Fatal("their own words answer their own question")
	}
	if a := <-got; a.Index != domain.ChoiceTyped || a.Text != "the b one" {
		t.Errorf("typed, trimmed: %+v", a)
	}
	if r.AnswerWithText(conv, "again") {
		t.Error("once answered, nothing is waiting")
	}
}
