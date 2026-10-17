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

// outbox records everything the platform sends to the person, in order.
type outbox struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (o *outbox) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, input map[string]any) (any, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, input)
	return "ok", nil
}
func (o *outbox) texts() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, s := range o.sent {
		out = append(out, s["text"].(string))
	}
	return out
}

func onboardingRig(t *testing.T, agent *domain.Agent) (*runtime.Host, *runtime.HostedAgent, *outbox) {
	t.Helper()
	box := &outbox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	return host, hosted, box
}

func firstMessage(text string) domain.InboundMessage {
	return domain.InboundMessage{
		Conversation: domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack"},
		Text:         text,
	}
}

// A new person's first message is welcomed AND answered, and the model has nothing to
// do with the welcome: the creator's own words go out exactly as written.
func TestOnboarding_TheFirstMessageIsWelcomedAndStillAnswered(t *testing.T) {
	llm := finishOnlyLLM("Here is your answer.")
	agent := &domain.Agent{ID: "a1", Name: "Desk", Greeting: "Welcome to the desk!", LLMs: []domain.LLM{llm}, MaxIterations: 5}
	host, hosted, box := onboardingRig(t, agent)

	completed := 0
	host.SetOnboardingFlow(
		func(context.Context, string, string, string) (bool, error) { return completed > 0, nil },
		func(context.Context, string, string, string, string, string) error { completed++; return nil },
	)

	host.DispatchInbound(context.Background(), hosted, firstMessage("what is the time"))

	got := box.texts()
	if len(got) != 2 || got[0] != "Welcome to the desk!" || got[1] != "Here is your answer." {
		t.Fatalf("welcome first, then the answer to what they wrote: %q", got)
	}
	if completed != 1 {
		t.Errorf("onboarding is completed once: %d", completed)
	}
	if llm.n != 1 {
		t.Errorf("the model answers the question and writes nothing else: %d calls", llm.n)
	}

	host.DispatchInbound(context.Background(), hosted, firstMessage("and again"))
	if got := box.texts(); len(got) != 3 || got[2] != "Here is your answer." {
		t.Errorf("the welcome is sent once only: %q", got)
	}
}

// Without a creator greeting the welcome is a fixed introduction built from the agent's
// own details: what it does is listed in the creator's words, for every ability, whether
// or not other agents may call it. Same every time, no model.
func TestOnboarding_TheDefaultWelcomeIsFixedText(t *testing.T) {
	llm := finishOnlyLLM("ok")
	agent := &domain.Agent{ID: "a1", Name: "Desk", Description: "Trades currencies.", LLMs: []domain.LLM{llm}, MaxIterations: 5,
		Skills: []*domain.Skill{
			{Name: "analyse_pair", Description: "Analyses a currency pair and rates the trade idea", Visibility: domain.SkillPrivate},
			{Name: "places_trades", Visibility: domain.SkillPrivate}, // no words from the creator: tidied name
		}}
	host, hosted, box := onboardingRig(t, agent)
	host.SetOnboardingFlow(
		func(context.Context, string, string, string) (bool, error) { return false, nil },
		func(context.Context, string, string, string, string, string) error { return nil },
	)
	host.DispatchInbound(context.Background(), hosted, firstMessage("hi"))
	want := "👋 Hi! I'm Desk.\n\nTrades currencies.\n\nHere's what I can help with:\n• Analyses a currency pair and rates the trade idea\n• Places Trades"
	if got := box.texts(); len(got) < 1 || got[0] != want {
		t.Fatalf("the default welcome:\n got %q\nwant %q", got, want)
	}
}

// On Telegram the welcome explains the button under it and carries it; the creator's own
// greeting still goes out word for word, with the same help appended.
func TestOnboarding_TheWelcomeExplainsAndCarriesTheGetStartedButton(t *testing.T) {
	for name, greeting := range map[string]string{"default": "", "creator's words": "Welcome aboard, trader."} {
		box := &outbox{}
		executors := domain.NewExecutorRegistry()
		executors.Register("telegram", box)
		deps := runtime.AgentDeps{
			IdentitiesByID:   map[string]*domain.Identity{"id_tg": {ID: "id_tg", IntegrationID: "int_tg"}},
			IntegrationsByID: map[string]*domain.Integration{"int_tg": {ID: "int_tg", Service: "telegram"}},
			Executors:        executors,
			Memory:           newMemMemory(),
		}
		agent := &domain.Agent{ID: "a1", Name: "Desk", Greeting: greeting, LLMs: []domain.LLM{finishOnlyLLM("ok")}, MaxIterations: 3}
		hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
		host := runtime.NewHost()
		host.Register(hosted)
		host.SetMiniAppBaseURL("https://app.example")
		host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
		host.SetOnboardingFlow(
			func(context.Context, string, string, string) (bool, error) { return false, nil },
			func(context.Context, string, string, string, string, string) error { return nil },
		)
		host.DispatchInbound(context.Background(), hosted, domain.InboundMessage{
			Conversation: domain.ConversationRef{Provider: "telegram", ChatID: "C1", IdentityID: "id_tg"}, Text: "hi",
		})
		if len(box.sent) == 0 {
			t.Fatalf("%s: nothing sent", name)
		}
		first := box.sent[0]["text"].(string)
		if !strings.Contains(first, "Tap Get started below to tell me your name and time zone") {
			t.Errorf("%s: the welcome must say what the button is for: %q", name, first)
		}
		if greeting != "" && !strings.HasPrefix(first, greeting) {
			t.Errorf("%s: the creator's words come first, unchanged: %q", name, first)
		}
		btn, _ := box.sent[0]["web_app_button"].(map[string]any)
		if btn["text"] != "Get started" || btn["url"] != "https://app.example/miniapp/welcome?agent=a1" {
			t.Errorf("%s: button: %v", name, box.sent[0]["web_app_button"])
		}
	}
}

// A message held up by a missing connection is not lost: the prompt is fixed text, and
// the moment the person connects, what they asked for is carried out.
func TestOnboarding_AMessageHeldForAConnectionIsReplayedOnceConnected(t *testing.T) {
	llm := finishOnlyLLM("Done: bought.")
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5, IdentityIDs: []string{"id_aegis"}}
	host, hosted, box := onboardingRig(t, agent)
	host.SetSetupChecker(func(context.Context, string, *domain.Agent) ([]string, error) { return []string{"id_aegis"}, nil })
	host.SetConnectURLGenerator(func(context.Context, string, string) (string, error) { return "https://app.example/connect", nil })
	host.SetConnectIntegrationNameLoader(func(context.Context, string) (string, error) { return "Aegis", nil })

	host.DispatchInbound(context.Background(), hosted, firstMessage("buy 10000 EURUSD"))

	got := box.texts()
	if len(got) != 1 || got[0] != "To go ahead, I need you to connect your Aegis account. Tap the button below." {
		t.Fatalf("the connect prompt is fixed text: %q", got)
	}
	if b, _ := box.sent[0]["web_app_button"].(map[string]any); b["text"] != "Connect Aegis account" {
		t.Errorf("button: %v", box.sent[0]["web_app_button"])
	}
	if llm.n != 0 {
		t.Fatalf("nothing is asked of the model before they connect: %d", llm.n)
	}

	host.OnIntegrationConnected(context.Background(), "u1", "id_aegis")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(box.texts()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := box.texts(); len(got) != 2 || !strings.Contains(got[1], "Done: bought.") {
		t.Fatalf("what they asked for runs once connected: %q", got)
	}
}

// ── getting started: held requests that survive a restart, local time, the button ──

// memStore is a PendingTurnStore shared between two hosts, to stand for the database
// that outlives a restart.
type memStore struct {
	mu    sync.Mutex
	turns map[string]*domain.PendingTurn
	saved time.Time // when the held request was kept; zero means now
}

func (m *memStore) SavePendingTurn(_ context.Context, t *domain.PendingTurn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns == nil {
		m.turns = map[string]*domain.PendingTurn{}
	}
	c := *t
	c.CreatedAt = time.Now()
	if !m.saved.IsZero() {
		c.CreatedAt = m.saved
	}
	m.turns[t.UserID+":"+t.IdentityID] = &c
	return nil
}
func (m *memStore) TakePendingTurn(_ context.Context, userID, identityID string, maxAge time.Duration) (*domain.PendingTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := userID + ":" + identityID
	t, ok := m.turns[k]
	if !ok {
		return nil, domain.ErrNotFound
	}
	delete(m.turns, k)
	if time.Since(t.CreatedAt) > maxAge {
		return nil, domain.ErrNotFound
	}
	return t, nil
}

func heldRig(t *testing.T, store *memStore) (*runtime.Host, *runtime.HostedAgent, *outbox, *funcLLMCount) {
	t.Helper()
	llm := &funcLLMCount{}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5, IdentityIDs: []string{"id_aegis"}}
	host, hosted, box := onboardingRig(t, agent)
	host.SetPendingTurnStore(store)
	host.SetSetupChecker(func(context.Context, string, *domain.Agent) ([]string, error) { return []string{"id_aegis"}, nil })
	host.SetConnectURLGenerator(func(context.Context, string, string) (string, error) { return "https://app.example/connect", nil })
	host.SetConnectIntegrationNameLoader(func(context.Context, string) (string, error) { return "Aegis", nil })
	return host, hosted, box, llm
}

type funcLLMCount struct {
	mu sync.Mutex
	n  int
}

func (f *funcLLMCount) Call(context.Context, []domain.Message, []domain.ActionSpec) (*domain.LLMResponse, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "Done: bought."}}}}, nil
}

func waitTexts(box *outbox, n int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(box.texts()) < n {
		time.Sleep(20 * time.Millisecond)
	}
	return box.texts()
}

// The server restarts between the person asking and connecting: what they asked for is
// still carried out.
func TestHeldRequest_SurvivesARestart(t *testing.T) {
	store := &memStore{}
	before, hostedBefore, _, _ := heldRig(t, store)
	before.DispatchInbound(context.Background(), hostedBefore, firstMessage("buy 10000 EURUSD"))

	after, _, box, llm := heldRig(t, store) // a fresh host: nothing in memory
	after.OnIntegrationConnected(context.Background(), "u1", "id_aegis")
	if got := waitTexts(box, 1); len(got) != 1 || got[0] != "Done: bought." {
		t.Fatalf("the request is carried out after a restart: %q", got)
	}
	if llm.n != 1 {
		t.Errorf("model calls: %d", llm.n)
	}
}

// An old request is not carried out as if it were fresh.
func TestHeldRequest_AStaleOneIsNotReplayed(t *testing.T) {
	store := &memStore{saved: time.Now().Add(-2 * time.Hour)}
	before, hostedBefore, _, _ := heldRig(t, store)
	before.DispatchInbound(context.Background(), hostedBefore, firstMessage("buy 10000 EURUSD"))

	after, _, box, llm := heldRig(t, store)
	after.OnIntegrationConnected(context.Background(), "u1", "id_aegis")
	time.Sleep(200 * time.Millisecond)
	if llm.n != 0 {
		t.Fatalf("a two-hour-old instruction must not run: %d model calls, sent %q", llm.n, box.texts())
	}
}

func TestSystemPrompt_SpeaksInThePersonsOwnTime(t *testing.T) {
	// handled in the internal test; here, the end-to-end: the loader's zone reaches the prompt.
	var seen string
	llm := &promptSpy{seen: &seen}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 3}
	host, hosted, _ := onboardingRig(t, agent)
	host.SetUserTimezoneLoader(func(context.Context, string) (string, error) { return "Africa/Lagos", nil })
	host.DispatchInbound(context.Background(), hosted, firstMessage("what day is it"))
	if !strings.Contains(seen, "Africa/Lagos") || !strings.Contains(seen, "the person's time zone") {
		t.Errorf("the agent must be told the time in the person's own zone: %q", seen)
	}
}

type promptSpy struct{ seen *string }

func (p *promptSpy) Call(_ context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	for _, m := range msgs {
		if m.Role == domain.RoleSystem {
			*p.seen = m.Content
		}
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
}

func TestConnectionsNeeded_ListsWhatIsMissingWithTheLinkToFixIt(t *testing.T) {
	host, _, _, _ := heldRig(t, &memStore{})
	got := host.ConnectionsNeeded(context.Background(), "u1", &domain.Agent{ID: "a1"})
	if len(got) != 1 || got[0].IdentityID != "id_aegis" || got[0].Name != "Aegis" || got[0].URL != "https://app.example/connect" {
		t.Errorf("connections needed: %+v", got)
	}
	if got := host.ConnectionsNeeded(context.Background(), "", &domain.Agent{ID: "a1"}); got != nil {
		t.Errorf("nobody, nothing to list: %+v", got)
	}
}
