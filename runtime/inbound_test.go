package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// liveExec is a messenger that delivers inbound messages the test injects (a Listener), records
// the replies it sends, can show questions with buttons, and takes typed answers to them.
type liveExec struct {
	outbox
	deliver chan func(domain.InboundMessage)
	buttons *buttonMessenger
	choices *runtime.MessengerChoiceRequester

	mu    sync.Mutex
	acked []string
}

func newLiveExec() *liveExec {
	b := &buttonMessenger{tokens: make(chan string, 8)}
	return &liveExec{deliver: make(chan func(domain.InboundMessage), 1), buttons: b, choices: runtime.NewMessengerChoiceRequester(b)}
}

func (l *liveExec) Listen(ctx context.Context, _ *domain.Identity, onMessage func(domain.InboundMessage)) error {
	l.deliver <- onMessage
	<-ctx.Done()
	return nil
}
func (l *liveExec) RequestChoice(ctx context.Context, conv domain.ConversationRef, q string, opts []string, secs int) (domain.ChoiceAnswer, error) {
	return l.choices.RequestChoice(ctx, nil, "", conv, q, opts, secs)
}
func (l *liveExec) SupersedeChoice(conv domain.ConversationRef) bool {
	return l.choices.Supersede(conv)
}
func (l *liveExec) AcknowledgeCallbackQuery(_ context.Context, queryID string) error {
	l.mu.Lock()
	l.acked = append(l.acked, queryID)
	l.mu.Unlock()
	return nil
}
func (l *liveExec) ackedIDs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.acked...)
}

// scriptLLM answers each turn from the last thing it was shown. A message containing "slow"
// waits until released; "ask" first puts a question to the person; everything else is answered
// at once. It records how many turns ran at the same moment.
type scriptLLM struct {
	started chan string
	release chan struct{}
	pause   time.Duration

	inflight, maxInflight atomic.Int32
	calls                 atomic.Int32
}

func (s *scriptLLM) Call(ctx context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	s.calls.Add(1)
	n := s.inflight.Add(1)
	for {
		m := s.maxInflight.Load()
		if n <= m || s.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	defer s.inflight.Add(-1)

	last := msgs[len(msgs)-1]
	say := func(text string) (*domain.LLMResponse, error) {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": text}}}}, nil
	}
	if last.Role == domain.RoleTool && last.Name == runtime.AskOptionsActionName {
		return say("got: " + last.Content)
	}
	if strings.Contains(last.Content, "ask") {
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "q", Name: runtime.AskOptionsActionName, Arguments: map[string]any{"question": "Which one?", "options": []any{"first", "second"}}}}}, nil
	}
	if strings.Contains(last.Content, "slow") {
		if s.started != nil {
			s.started <- last.Content
		}
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	}
	if s.pause > 0 {
		time.Sleep(s.pause)
	}
	return say("done: " + last.Content)
}

type liveRig struct {
	host   *runtime.Host
	exec   *liveExec
	mem    *memMemory
	llm    *scriptLLM
	send   func(chat, text string)
	raw    func(domain.InboundMessage)
	cancel context.CancelFunc
}

func newLiveRig(t *testing.T, llm *scriptLLM) *liveRig {
	t.Helper()
	ex := newLiveExec()
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", ex)
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5, IdentityIDs: []string{"id_slack"}}
	mem := newMemMemory()
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = mem
	})}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { host.ListenAndServe(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the host did not shut down")
		}
	})
	var onMessage func(domain.InboundMessage)
	select {
	case onMessage = <-ex.deliver:
	case <-time.After(3 * time.Second):
		t.Fatal("the listener never started")
	}
	return &liveRig{host: host, exec: ex, mem: mem, llm: llm, cancel: cancel, raw: onMessage, send: func(chat, text string) {
		onMessage(domain.InboundMessage{
			Conversation: domain.ConversationRef{Provider: "slack", ChatID: chat, IdentityID: "id_slack", SenderID: "U-" + chat},
			Text:         text,
		})
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func has(texts []string, want string) bool {
	for _, x := range texts {
		if strings.Contains(x, want) {
			return true
		}
	}
	return false
}

// The point of all this: one conversation stuck on something slow must not hold up anyone else.
func TestInbound_ASlowConversationDoesNotHoldUpAnother(t *testing.T) {
	llm := &scriptLLM{started: make(chan string, 4), release: make(chan struct{})}
	r := newLiveRig(t, llm)

	r.send("C1", "slow one")
	<-llm.started // C1's turn is now stuck
	r.send("C2", "quick one")

	waitFor(t, "the other conversation to be answered while C1 is stuck", func() bool { return has(r.exec.texts(), "done: quick one") })
	if has(r.exec.texts(), "done: slow one") {
		t.Fatal("the slow conversation is still stuck")
	}
	close(llm.release)
	waitFor(t, "the slow conversation to finish once released", func() bool { return has(r.exec.texts(), "done: slow one") })
}

// Within ONE conversation order matters (each turn builds on the history of the one before):
// messages are handled strictly in turn, never two at once.
func TestInbound_MessagesInOneConversationStayInOrderAndNeverOverlap(t *testing.T) {
	llm := &scriptLLM{pause: 30 * time.Millisecond}
	r := newLiveRig(t, llm)
	for i := 1; i <= 4; i++ {
		r.send("C1", fmt.Sprintf("msg %d", i))
	}
	waitFor(t, "all four answers", func() bool { return len(r.exec.texts()) == 4 })
	got := r.exec.texts()
	for i, want := range []string{"done: msg 1", "done: msg 2", "done: msg 3", "done: msg 4"} {
		if got[i] != want {
			t.Fatalf("answers out of order: %q", got)
		}
	}
	if m := llm.maxInflight.Load(); m != 1 {
		t.Errorf("one conversation is never handled two at a time: %d at once", m)
	}
}

// Different conversations really do run side by side.
func TestInbound_DifferentConversationsRunAtTheSameTime(t *testing.T) {
	llm := &scriptLLM{pause: 150 * time.Millisecond}
	r := newLiveRig(t, llm)
	for _, chat := range []string{"C1", "C2", "C3"} {
		r.send(chat, "hello from "+chat)
	}
	waitFor(t, "three answers", func() bool { return len(r.exec.texts()) == 3 })
	if m := llm.maxInflight.Load(); m < 2 {
		t.Errorf("different conversations must overlap: at most %d at once", m)
	}
}

// Writing while a question is waiting cuts the question short: its turn ends quietly (no stray
// reply), and what was written is handled next as a message that follows the question, so the
// agent can tell an answer from a new request.
func TestInbound_WritingWhileAQuestionWaitsEndsItAndTheMessageIsHandledNext(t *testing.T) {
	llm := &scriptLLM{}
	r := newLiveRig(t, llm)

	r.send("C1", "please ask me")
	select {
	case <-r.exec.buttons.tokens: // the question is on screen
	case <-time.After(3 * time.Second):
		t.Fatal("the question was never posted")
	}
	r.send("C1", "the second one, please")

	waitFor(t, "the written message to be answered", func() bool { return has(r.exec.texts(), "done: the second one, please") })
	if got := r.exec.texts(); len(got) != 1 {
		t.Errorf("only the new message gets a reply, the cut-short turn says nothing: %q", got)
	}
	if n := llm.calls.Load(); n != 2 {
		t.Errorf("one call to ask, one to answer the new message: %d", n)
	}
	r.exec.buttons.mu.Lock()
	if !strings.Contains(r.exec.buttons.updated, "Replaced by your new message") {
		t.Errorf("the question message shows it was replaced: %q", r.exec.buttons.updated)
	}
	r.exec.buttons.mu.Unlock()

	// The new message follows the unanswered question in the history, so it can be read in context.
	var hist []domain.Message
	for _, msgs := range r.mem.store {
		hist = msgs
	}
	var sawQuestion, sawUnanswered, sawNew bool
	for _, m := range hist {
		sawQuestion = sawQuestion || len(m.ToolCalls) > 0 && m.ToolCalls[0].Name == runtime.AskOptionsActionName
		sawUnanswered = sawUnanswered || m.Role == domain.RoleTool && strings.Contains(m.Content, "not answered")
		sawNew = sawNew || m.Role == domain.RoleUser && m.Content == "the second one, please"
	}
	if !sawQuestion || !sawUnanswered || !sawNew {
		t.Errorf("history keeps the question, that it went unanswered, and the new message: %v %v %v", sawQuestion, sawUnanswered, sawNew)
	}
}

// A button tap is never queued behind a turn: it may be exactly what that turn is waiting for.
func TestInbound_AButtonTapIsNotHeldUpByATurnInProgress(t *testing.T) {
	llm := &scriptLLM{started: make(chan string, 4), release: make(chan struct{})}
	r := newLiveRig(t, llm)
	r.send("C1", "slow one")
	<-llm.started // C1's turn is stuck

	r.raw(domain.InboundMessage{
		Conversation:  domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", SenderID: "U-C1"},
		CallbackQuery: &domain.CallbackQueryData{QueryID: "q-1", Data: "kael_sm:account_linked"},
	})
	waitFor(t, "the tap to be handled while the turn is still stuck", func() bool {
		for _, id := range r.exec.ackedIDs() {
			if id == "q-1" {
				return true
			}
		}
		return false
	})
	if has(r.exec.texts(), "done: slow one") {
		t.Fatal("the turn must still be stuck")
	}
	close(llm.release)
}
