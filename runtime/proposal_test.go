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

// memProposals keeps choices in memory with the same rules as the real store: one tap wins, and
// they expire.
type memProposals struct {
	mu sync.Mutex
	m  map[string]*domain.Proposal
}

func newMemProposals() *memProposals { return &memProposals{m: map[string]*domain.Proposal{}} }

func (s *memProposals) SaveProposal(_ context.Context, p *domain.Proposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *p
	s.m[p.ID] = &c
	return nil
}
func (s *memProposals) GetProposal(_ context.Context, id string) (*domain.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c := *p
	return &c, nil
}
func (s *memProposals) ChooseProposal(_ context.Context, id string, i int, now time.Time) (*domain.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[id]
	if !ok || i < 0 || i >= len(p.Choices) {
		return nil, domain.ErrNotFound
	}
	if p.Chosen != nil {
		return nil, domain.ErrProposalChosen
	}
	if !now.Before(p.ExpiresAt) {
		return nil, domain.ErrProposalExpired
	}
	p.Chosen = &i
	c := *p
	return &c, nil
}
func (s *memProposals) DeleteExpiredProposals(context.Context, time.Time) (int, error) { return 0, nil }

// buttonOutbox is a messenger that can put buttons under a message and edit it.
type buttonOutbox struct {
	outbox
	mu2     sync.Mutex
	buttons [][]domain.Button
	onMsg   []string
	edits   []string
	acks    []string
}

func (b *buttonOutbox) Execute(ctx context.Context, i *domain.Identity, c string, a string, in map[string]any) (any, error) {
	_, _ = b.outbox.Execute(ctx, i, c, a, in)
	return map[string]any{"message_id": "m1"}, nil
}
func (b *buttonOutbox) AttachButtons(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, messageID string, buttons []domain.Button) error {
	b.mu2.Lock()
	defer b.mu2.Unlock()
	b.buttons = append(b.buttons, buttons)
	b.onMsg = append(b.onMsg, messageID)
	return nil
}
func (b *buttonOutbox) UpdateMessage(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, _ string, text string) error {
	b.mu2.Lock()
	defer b.mu2.Unlock()
	b.edits = append(b.edits, text)
	return nil
}
func (b *buttonOutbox) AcknowledgeCallbackQuery(_ context.Context, id string) error {
	b.mu2.Lock()
	defer b.mu2.Unlock()
	b.acks = append(b.acks, id)
	return nil
}
func (b *buttonOutbox) PostApprovalPrompt(context.Context, *domain.Identity, string, domain.ConversationRef, string, string) (string, error) {
	return "", nil
}
func (b *buttonOutbox) snapshot() (buttons [][]domain.Button, edits []string) {
	b.mu2.Lock()
	defer b.mu2.Unlock()
	return append([][]domain.Button(nil), b.buttons...), append([]string(nil), b.edits...)
}

// finishWithChoices ends the turn with a spoken reply and choices, then (on later turns) a plain reply.
func finishWithChoices(first string, choices []any, then string) *funcLLM {
	return &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		args := map[string]any{"content": then}
		if n == 1 {
			args = map[string]any{"content": first, "choices": choices}
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: args}}}, nil
	}}
}

func twoChoices() []any {
	return []any{
		map[string]any{"label": "10:15 tomorrow", "say": "Move my dentist appointment to 10:15 tomorrow."},
		map[string]any{"label": "Keep it", "say": "Keep my dentist appointment as it is."},
	}
}

func choiceRig(t *testing.T, llm domain.LLM, withStore bool) (*runtime.Host, *runtime.HostedAgent, *buttonOutbox, *memProposals) {
	t.Helper()
	box := &buttonOutbox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
	})
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5}, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	store := newMemProposals()
	if withStore {
		host.SetProposalStore(store)
	}
	return host, hosted, box, store
}

func tap(data string) domain.InboundMessage {
	return domain.InboundMessage{
		Conversation:  domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack"},
		CallbackQuery: &domain.CallbackQueryData{QueryID: "q1", Data: data, MessageID: "m1"},
	}
}

func TestChoices_AreSavedAndShownAsButtonsUnderTheMessage(t *testing.T) {
	host, hosted, box, store := choiceRig(t, finishWithChoices("Your dentist clashes with the stand-up.", twoChoices(), "ok"), true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("what is on tomorrow"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })

	buttons, _ := box.snapshot()
	if len(buttons[0]) != 2 || buttons[0][0].Label != "10:15 tomorrow" || buttons[0][1].Label != "Keep it" {
		t.Fatalf("two buttons, as given: %+v", buttons[0])
	}
	if got := box.texts(); len(got) != 1 || got[0] != "Your dentist clashes with the stand-up." {
		t.Errorf("the message itself carries no list when there are buttons: %q", got)
	}
	id, i, ok := domain.ParseProposalData(buttons[0][1].Data)
	if !ok || i != 1 {
		t.Fatalf("button data: %q", buttons[0][1].Data)
	}
	p, err := store.GetProposal(context.Background(), id)
	if err != nil || p.PersonID != "u1" || p.AgentID != "a1" || p.MessageID != "m1" || p.ChatID != "C1" || len(p.Choices) != 2 {
		t.Fatalf("saved for this person, agent and message: %+v %v", p, err)
	}
	if !p.ExpiresAt.After(time.Now().Add(23 * time.Hour)) {
		t.Errorf("lasts a day: %v", p.ExpiresAt)
	}
}

func TestChoices_AreAPlainNumberedListWhereButtonsCannotWork(t *testing.T) {
	// No saved options turned on: the same reply, as a list.
	host, hosted, box, _ := choiceRig(t, finishWithChoices("Your dentist clashes.", twoChoices(), "ok"), false)
	host.DispatchInbound(context.Background(), hosted, firstMessage("what is on tomorrow"))
	waitFor(t, "reply", func() bool { return len(box.texts()) == 1 })
	if got := box.texts()[0]; got != "Your dentist clashes.\n\n1. 10:15 tomorrow\n2. Keep it" {
		t.Errorf("numbered list: %q", got)
	}
	if b, _ := box.snapshot(); len(b) != 0 {
		t.Errorf("no buttons: %v", b)
	}
}

func TestChoices_ATapIsHandledAsIfTheyTypedIt(t *testing.T) {
	llm := finishWithChoices("Your dentist clashes.", twoChoices(), "Moved it to 10:15.")
	host, hosted, box, store := choiceRig(t, llm, true)
	host.DispatchInbound(context.Background(), hosted, firstMessage("what is on tomorrow"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	buttons, _ := box.snapshot()

	host.DispatchInbound(context.Background(), hosted, tap(buttons[0][0].Data))
	waitFor(t, "answer to the tap", func() bool { return len(box.texts()) == 2 })

	if got := box.texts()[1]; got != "Moved it to 10:15." {
		t.Errorf("the tap got a normal turn: %q", got)
	}
	_, edits := box.snapshot()
	if len(edits) != 1 || edits[0] != "Your dentist clashes.\n\n✅ 10:15 tomorrow" {
		t.Errorf("the message shows what was chosen: %q", edits)
	}
	if len(box.acks) != 1 || box.acks[0] != "q1" {
		t.Errorf("the tap is acknowledged: %v", box.acks)
	}
	id, _, _ := domain.ParseProposalData(buttons[0][0].Data)
	if p, _ := store.GetProposal(context.Background(), id); p.Chosen == nil || *p.Chosen != 0 {
		t.Errorf("recorded: %+v", p)
	}
}

func TestChoices_TheSentenceBehindTheButtonIsWhatTheAgentHears(t *testing.T) {
	var heard []string
	var mu sync.Mutex
	host, hosted, box, _ := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "ok"), true)
	var got string
	host.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) {
		mu.Lock()
		defer mu.Unlock()
		got = m.Text
		heard = append(heard, m.Conversation.UserID)
	})
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	buttons, _ := box.snapshot()
	host.DispatchInbound(context.Background(), hosted, tap(buttons[0][1].Data))
	waitFor(t, "dispatch", func() bool { mu.Lock(); defer mu.Unlock(); return got != "" })
	if got != "Keep my dentist appointment as it is." || heard[0] != "u1" {
		t.Errorf("heard %q from %v", got, heard)
	}
}

func TestChoices_AreAnsweredOnceExpireAndBelongToOnePerson(t *testing.T) {
	host, hosted, box, store := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "done"), true)
	dispatched := make(chan string, 8)
	host.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) { dispatched <- m.Text })
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	buttons, _ := box.snapshot()
	data := buttons[0][0].Data

	// A second tap, on any button, does nothing.
	host.DispatchInbound(context.Background(), hosted, tap(data))
	waitFor(t, "first tap", func() bool { return len(dispatched) == 1 })
	host.DispatchInbound(context.Background(), hosted, tap(data))
	host.DispatchInbound(context.Background(), hosted, tap(buttons[0][1].Data))
	time.Sleep(100 * time.Millisecond)
	if len(dispatched) != 1 {
		t.Errorf("answered once: %d", len(dispatched))
	}

	// Expired: the buttons say so and nothing is done.
	host2, hosted2, box2, store2 := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "done"), true)
	disp2 := make(chan string, 2)
	host2.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) { disp2 <- m.Text })
	host2.DispatchInbound(context.Background(), hosted2, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box2.snapshot(); return len(b) == 1 })
	b2, _ := box2.snapshot()
	id, _, _ := domain.ParseProposalData(b2[0][0].Data)
	store2.mu.Lock()
	store2.m[id].ExpiresAt = time.Now().Add(-time.Minute)
	store2.mu.Unlock()
	host2.DispatchInbound(context.Background(), hosted2, tap(b2[0][0].Data))
	waitFor(t, "expiry notice", func() bool { _, e := box2.snapshot(); return len(e) == 1 })
	if _, e := box2.snapshot(); !strings.Contains(e[0], "expired") {
		t.Errorf("says so: %q", e)
	}
	if len(disp2) != 0 {
		t.Error("an expired choice does nothing")
	}

	// Somebody else's tap, or another agent's, is ignored.
	host3, hosted3, box3, _ := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "done"), true)
	disp3 := make(chan string, 2)
	host3.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) { disp3 <- m.Text })
	host3.DispatchInbound(context.Background(), hosted3, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box3.snapshot(); return len(b) == 1 })
	b3, _ := box3.snapshot()
	host3.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "someone-else", nil })
	host3.DispatchInbound(context.Background(), hosted3, tap(b3[0][0].Data))
	time.Sleep(100 * time.Millisecond)
	if len(disp3) != 0 {
		t.Error("only the person it was for can tap")
	}

	// Another agent, same person: its tap on these buttons is ignored too.
	host4, hosted4, box4, _ := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "done"), true)
	disp4 := make(chan string, 2)
	host4.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) { disp4 <- m.Text })
	host4.DispatchInbound(context.Background(), hosted4, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box4.snapshot(); return len(b) == 1 })
	b4, _ := box4.snapshot()
	other := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a2", Name: "Other", LLMs: hosted4.Agent.LLMs, MaxIterations: 5}, Deps: hosted4.Deps}
	host4.Register(other)
	host4.DispatchInbound(context.Background(), other, tap(b4[0][0].Data))
	time.Sleep(100 * time.Millisecond)
	if len(disp4) != 0 {
		t.Error("only the agent that made the choices takes the tap")
	}
	_ = store
}

func TestChoices_NotOfferedWhenNobodyIsThereToTap(t *testing.T) {
	host, hosted, box, store := choiceRig(t, finishWithChoices("Summary.", twoChoices(), "ok"), true)
	if err := host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "check my calendar"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reply", func() bool { return len(box.texts()) == 1 })
	if got := box.texts()[0]; got != "Summary." {
		t.Errorf("an unattended run says its piece, with no list or buttons: %q", got)
	}
	if b, _ := box.snapshot(); len(b) != 0 || len(store.m) != 0 {
		t.Errorf("nothing kept: %v %d", b, len(store.m))
	}
	_ = hosted
}

// An alert reaches the person on purpose, so its run may end with choices ("Place the trade"): saved for that person
// and agent, and a tap is handled as if they had typed it, like any other.
func TestChoices_AnAlertRunMayOfferThem(t *testing.T) {
	host, _, box, store := choiceRig(t, finishWithChoices("New buy signal on EURUSD.", twoChoices(), "ok"), true)
	ctx := domain.WithAlertRun(context.Background())
	if err := host.RunScheduledTask(ctx, "a1", "u1", "id_slack", "C1", "A new trading signal was made"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	if len(store.m) != 1 {
		t.Fatalf("the choices are saved: %d", len(store.m))
	}
	for _, p := range store.m {
		if p.PersonID != "u1" || p.AgentID != "a1" {
			t.Errorf("for this person and agent: %+v", p)
		}
	}
}

// A write-only run offers the model nothing but finish, whatever capabilities the agent has.
func TestWriteOnly_OffersNothingButFinish(t *testing.T) {
	spy := &toolSpy{}
	r := newFollowRig(t, &followRouter{byText: map[string][]string{"A new signal": {"analyse_pair"}}})
	r.hosted.Agent.LLMs = []domain.LLM{spy}
	ctx := domain.WithWriteOnly(domain.WithUnattended(context.Background()))
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	if _, err := r.host.HandleTurn(ctx, r.hosted, conv, "A new signal"); err != nil {
		t.Fatal(err)
	}
	if spy.calls() != 1 || strings.Join(spy.offered[0], ",") != domain.FinishActionName {
		t.Fatalf("one call, only finish: %v", spy.offered)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != "ok" {
		t.Errorf("its reply is delivered: %q", got)
	}
}

// A write-only run that fails sends nothing itself: the caller sends something plainer (an alert's summary).
func TestWriteOnly_AFailureSendsNothing(t *testing.T) {
	host, _, box, _ := choiceRig(t, &funcLLM{call: func(int) (*domain.LLMResponse, error) { return nil, errors.New("model down") }}, true)
	got := make(chan error, 1)
	ctx := domain.WithWriteOnly(domain.WithAlertRun(context.Background()))
	if err := host.RunScheduledTaskThen(ctx, "a1", "u1", "id_slack", "C1", "A new signal", func(err error) { got <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-got; err == nil {
		t.Fatal("the caller is told it failed")
	}
	if texts := box.texts(); len(texts) != 0 {
		t.Errorf("nothing sent: %q", texts)
	}
}

// finderProposals also finds the newest open choices in a chat (a store that can, like Postgres).
type finderProposals struct{ *memProposals }

func (s finderProposals) LatestOpenProposal(_ context.Context, agentID, identityID, chatID string, now time.Time) (*domain.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *domain.Proposal
	for _, p := range s.m {
		if p.AgentID == agentID && p.IdentityID == identityID && p.ChatID == chatID && p.Chosen == nil && now.Before(p.ExpiresAt) &&
			(best == nil || p.ExpiresAt.After(best.ExpiresAt)) {
			best = p
		}
	}
	if best == nil {
		return nil, domain.ErrNotFound
	}
	c := *best
	return &c, nil
}

// A choice sent back as the person's own words (Telegram's reply keyboard) is that choice: the sentence behind it is
// what the agent hears, once; other words are an ordinary message.
func TestChoices_TheirWordsAreTheChoice(t *testing.T) {
	host, hosted, box, mem := choiceRig(t, finishWithChoices("Clash.", twoChoices(), "ok"), false)
	host.SetProposalStore(finderProposals{mem})
	var mu sync.Mutex
	var heard []string
	host.SetProposalDispatch(func(_ context.Context, _ *runtime.HostedAgent, m domain.InboundMessage) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, m.Text)
	})
	host.DispatchInbound(context.Background(), hosted, firstMessage("go"))
	waitFor(t, "buttons", func() bool { b, _ := box.snapshot(); return len(b) == 1 })
	host.DispatchInbound(context.Background(), hosted, firstMessage(" keep it "))
	waitFor(t, "dispatch", func() bool { mu.Lock(); defer mu.Unlock(); return len(heard) == 1 })
	if heard[0] != "Keep my dentist appointment as it is." {
		t.Errorf("heard %q", heard)
	}
	// Chosen now: the same words again are an ordinary message (answered by the agent, not taken as a choice).
	host.DispatchInbound(context.Background(), hosted, firstMessage("Keep it"))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 {
		t.Errorf("once: %q", heard)
	}
}
