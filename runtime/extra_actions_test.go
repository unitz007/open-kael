package runtime_test

import (
	"context"
	"github.com/unitz007/open-kael/runtime"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// The application can add actions to a person's turn; they are offered to the model alongside the agent's own.
func TestExtraActions_AreOfferedOnAPersonsTurn(t *testing.T) {
	r := newFollowRig(t, &followRouter{byText: map[string][]string{"what is the price": {"analyse_pair"}}})
	r.host.SetExtraActions(func(context.Context, *runtime.HostedAgent) []*domain.BoundAction {
		return []*domain.BoundAction{{Spec: domain.ActionSpec{Name: "set_up_thing", Description: "Sets something up", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}}}
	})
	r.say(t, "C1", "what is the price")
	if r.spy.calls() == 0 {
		t.Fatal("the model was not called")
	}
	if got := strings.Join(r.spy.offered[0], ","); !strings.Contains(got, "set_up_thing") {
		t.Errorf("the extra action is offered: %s", got)
	}
}

// RunScheduledTaskThen tells the caller when the run is over, with whether it failed.
func TestRunScheduledTaskThen_CallsBackWhenTheRunIsOver(t *testing.T) {
	r := newFollowRig(t, &followRouter{byText: map[string][]string{}})
	done := make(chan error, 1)
	if err := r.host.RunScheduledTaskThen(context.Background(), "a1", "u1", "id_slack", "C1", "what is the price", func(err error) { done <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the run finished: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never called back")
	}
}
