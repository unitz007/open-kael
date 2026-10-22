package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("no signal: %s", what)
	}
}

// While an agent is handling a message it is working; watchers are told when it starts and when it stops, and only the
// agents they watch.
func TestWork_AnAgentIsWorkingWhileItHandlesAMessage(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	llm := &funcLLM{call: func(int) (*domain.LLMResponse, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil, errors.New("done for the test")
	}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 1}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &outbox{})
	host := runtime.NewHost()
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })}
	host.Register(hosted)

	if got := host.Working([]string{"a1"}); len(got) != 0 {
		t.Fatalf("idle at first: %v", got)
	}
	mine, stop := host.WatchWork([]string{"a1"})
	defer stop()
	other, stopOther := host.WatchWork([]string{"someone-else"})
	defer stopOther()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
		_, _ = host.HandleTurn(context.Background(), hosted, conv, "hello there")
	}()
	<-entered
	waitSignal(t, mine, "started")
	if got := host.Working([]string{"a1", "b2"}); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("working while the turn runs: %v", got)
	}
	close(release)
	<-done
	waitSignal(t, mine, "stopped")
	if got := host.Working([]string{"a1"}); len(got) != 0 {
		t.Fatalf("idle once it is over: %v", got)
	}
	select {
	case <-other:
		t.Fatal("someone watching another agent was told")
	default:
	}
}
