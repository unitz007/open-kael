package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func inbound(r *followRig, text string) {
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	r.host.HandleInbound(context.Background(), r.hosted, domain.InboundMessage{Conversation: conv, Text: text})
}

// An agent that is not ready runs nothing and says so in plain words, without the creator's reason.
func TestNotReadyAgent_SaysSoAndRunsNothing(t *testing.T) {
	r := newFollowRig(t, &followRouter{byText: map[string][]string{}})
	r.host.SetReadinessCheck(func(context.Context, *domain.Agent) string { return "Add a capability with a connection." })
	inbound(r, "what is the price")
	if r.spy.calls() != 0 {
		t.Errorf("the model was called %d time(s) for an agent that is not ready", r.spy.calls())
	}
	got := r.box.texts()
	if len(got) != 1 || !strings.Contains(got[0], "not ready to help yet") || strings.Contains(got[0], "capability") {
		t.Errorf("the person is told it is not ready, nothing more: %v", got)
	}
}

// A button tap is held to it too: a tap is a message in disguise.
func TestNotReadyAgent_ATapDoesNotGetPastIt(t *testing.T) {
	r := newFollowRig(t, &followRouter{byText: map[string][]string{}})
	r.host.SetReadinessCheck(func(context.Context, *domain.Agent) string { return "no" })
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	r.host.HandleInbound(context.Background(), r.hosted, domain.InboundMessage{Conversation: conv, CallbackQuery: &domain.CallbackQueryData{Data: "prop:x:0"}})
	if r.spy.calls() != 0 || len(r.box.texts()) != 1 {
		t.Errorf("calls=%d texts=%v", r.spy.calls(), r.box.texts())
	}
}

// A ready agent, and a host with no check at all, behave as before.
func TestReadyAgent_AnswersAsBefore(t *testing.T) {
	for name, check := range map[string]func(context.Context, *domain.Agent) string{
		"ready":    func(context.Context, *domain.Agent) string { return "" },
		"no check": nil,
	} {
		r := newFollowRig(t, &followRouter{byText: map[string][]string{"what is the price": {"analyse_pair"}}})
		if check != nil {
			r.host.SetReadinessCheck(check)
		}
		inbound(r, "what is the price")
		if r.spy.calls() == 0 {
			t.Errorf("%s: the agent did not answer", name)
		}
		for _, s := range r.box.texts() {
			if strings.Contains(s, "not ready to help yet") {
				t.Errorf("%s: wrongly told it is not ready", name)
			}
		}
	}
}

var _ = runtime.NewHost
