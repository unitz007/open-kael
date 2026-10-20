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

// listeningBox is a chat app that can be listened to: it keeps the function that hands it messages, and records what is said back.
type listeningBox struct {
	outbox
	mu      sync.Mutex
	deliver func(domain.InboundMessage)
}

func (l *listeningBox) Listen(ctx context.Context, _ *domain.Identity, onMessage func(domain.InboundMessage)) error {
	l.mu.Lock()
	l.deliver = onMessage
	l.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (l *listeningBox) send(m domain.InboundMessage) bool {
	l.mu.Lock()
	f := l.deliver
	l.mu.Unlock()
	if f == nil {
		return false
	}
	f(m)
	return true
}

func lateRig(t *testing.T) (*runtime.Host, *runtime.HostedAgent, *listeningBox, context.CancelFunc) {
	t.Helper()
	box := &listeningBox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	agent := &domain.Agent{ID: "a1", Name: "Late", LLMs: []domain.LLM{finishOnlyLLM("hello there")}, MaxIterations: 5} // no chat app yet
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
	})}
	host := runtime.NewHost()
	host.Register(hosted)
	ctx, cancel := context.WithCancel(context.Background())
	go host.ListenAndServe(ctx)
	time.Sleep(100 * time.Millisecond) // it is serving, and this agent has no chat app to listen on
	return host, hosted, box, cancel
}

func waitUntilTrue(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An agent that had no chat app when the server started must answer once one is attached: its inbox used to be
// left behind with no one reading it, so every message to it waited unread until the next restart.
func TestAChatAppAttachedToARunningAgentThatHadNoneIsAnswered(t *testing.T) {
	host, hosted, box, cancel := lateRig(t)
	defer cancel()
	identity, integration := &domain.Identity{ID: "id_slack", IntegrationID: "int_slack"}, &domain.Integration{ID: "int_slack", Service: "slack"}
	host.StartListenerForAgent(hosted.Agent.ID, identity, integration)

	waitUntilTrue(t, "the listener", func() bool { box.mu.Lock(); defer box.mu.Unlock(); return box.deliver != nil })

	box.send(domain.InboundMessage{Conversation: domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}, Text: "hi"})
	waitUntilTrue(t, "an answer", func() bool {
		for _, s := range box.texts() {
			if strings.Contains(s, "hello there") {
				return true
			}
		}
		return false
	})
}

// A person who needs to connect an account that no sign-in can be offered for is told so, in plain words.
func TestAMissingConnectionWithNoSignInIsSaidNotLeftInSilence(t *testing.T) {
	r := newFollowRig(t, &followRouter{byText: map[string][]string{}})
	r.host.SetSetupChecker(func(context.Context, string, *domain.Agent) ([]string, error) { return []string{"id-gh"}, nil })
	r.host.SetConnectURLGenerator(func(context.Context, string, string) (string, error) { return "", context.DeadlineExceeded })
	r.host.SetConnectIntegrationNameLoader(func(context.Context, string) (string, error) { return "GitHub", nil })
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	r.host.HandleInbound(context.Background(), r.hosted, domain.InboundMessage{Conversation: conv, Text: "what is the price"})
	got := r.box.texts()
	if len(got) != 1 || !strings.Contains(got[0], "connect your GitHub account") || !strings.Contains(got[0], "can't offer the sign-in right now") {
		t.Fatalf("they are told what is needed and that it cannot be offered: %q", got)
	}
	if r.spy.calls() != 0 {
		t.Error("the agent does not carry on without the connection")
	}
}
