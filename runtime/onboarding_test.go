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
// own details, the same every time and written without a model.
func TestOnboarding_TheDefaultWelcomeIsFixedText(t *testing.T) {
	llm := finishOnlyLLM("ok")
	agent := &domain.Agent{ID: "a1", Name: "Desk", Description: "Trades currencies.", LLMs: []domain.LLM{llm}, MaxIterations: 5,
		Skills: []*domain.Skill{{Name: "places_trades", Description: "Places trades.", Visibility: domain.SkillPublic}}}
	host, hosted, box := onboardingRig(t, agent)
	host.SetOnboardingFlow(
		func(context.Context, string, string, string) (bool, error) { return false, nil },
		func(context.Context, string, string, string, string, string) error { return nil },
	)
	host.DispatchInbound(context.Background(), hosted, firstMessage("hi"))
	want := "👋 Hi! I'm Desk.\n\nTrades currencies.\n\nHere's what I can help you with:\n• Places Trades — Places trades."
	if got := box.texts(); len(got) < 1 || got[0] != want {
		t.Fatalf("the default welcome:\n got %q\nwant %q", got, want)
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
