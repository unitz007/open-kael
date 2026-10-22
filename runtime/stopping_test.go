package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// heldLLM answers only once it is let go, or gives up when its context ends: a reply that is still being written.
type heldLLM struct {
	started chan struct{}
	release chan struct{}
}

func (l *heldLLM) Call(ctx context.Context, _ []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	select {
	case l.started <- struct{}{}:
	default:
	}
	select {
	case <-l.release:
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "here you go"}}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func stoppingRig(t *testing.T) (*runtime.Host, *listeningBox, *heldLLM, context.CancelFunc, chan struct{}) {
	t.Helper()
	box := &listeningBox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	llm := &heldLLM{started: make(chan struct{}, 1), release: make(chan struct{})}
	agent := &domain.Agent{ID: "a1", Name: "Steady", IdentityIDs: []string{"id_slack"}, LLMs: []domain.LLM{llm}, MaxIterations: 5}
	host := runtime.NewHost()
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
	})})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = host.ListenAndServe(ctx); close(stopped) }()
	waitUntilTrue(t, "the listener", func() bool { box.mu.Lock(); defer box.mu.Unlock(); return box.deliver != nil })
	box.send(domain.InboundMessage{Conversation: domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}, Text: "what's on today?"})
	select {
	case <-llm.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the message was never handled")
	}
	return host, box, llm, cancel, stopped
}

func said(box *listeningBox, text string) bool {
	for _, s := range box.texts() {
		if s == text {
			return true
		}
	}
	return false
}

// A server stopping for a deploy used to cancel a reply half way: the person got nothing. Now the reply being written
// when it begins to stop still reaches them, and the server waits for it.
func TestStopping_AReplyBeingWrittenStillReachesThePerson(t *testing.T) {
	_, box, llm, cancel, stopped := stoppingRig(t)
	cancel() // the server begins to stop while the reply is being written
	select {
	case <-stopped:
		t.Fatal("stopped without waiting for the reply")
	case <-time.After(150 * time.Millisecond):
	}
	close(llm.release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("never finished stopping")
	}
	if !said(box, "here you go") {
		t.Errorf("the reply was not delivered: %v", box.texts())
	}
}

// When the grace for stopping runs out, CancelTurns ends what is still going, and the server stops.
func TestStopping_CancelTurnsEndsWhatIsStillGoing(t *testing.T) {
	host, box, _, cancel, stopped := stoppingRig(t)
	cancel()
	host.CancelTurns()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("never finished stopping")
	}
	if said(box, "here you go") {
		t.Error("a cancelled reply was delivered")
	}
}
