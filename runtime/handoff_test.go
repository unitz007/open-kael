package runtime_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A message the handoff takes is answered by it alone, before the welcome or the agent; any other goes on as before.
func TestHandoff_TakesItsMessagesFirst(t *testing.T) {
	agent := &domain.Agent{ID: "a1", Name: "Desk", Greeting: "Welcome to the desk!", LLMs: []domain.LLM{finishOnlyLLM("Here is your answer.")}, MaxIterations: 5}
	host, hosted, box := onboardingRig(t, agent)
	done := false
	host.SetOnboardingFlow(
		func(context.Context, string, string, string) (bool, error) { return done, nil },
		func(context.Context, string, string, string, string, string) error { done = true; return nil },
	)
	var heard domain.ConversationRef
	host.SetHandoff(func(_ context.Context, conv domain.ConversationRef, agentID, text string, say func(string)) bool {
		if text != "/start k123" || agentID != "a1" {
			return false
		}
		heard = conv
		say("Carried over.")
		return true
	})
	host.DispatchInbound(context.Background(), hosted, firstMessage("/start k123"))
	if got := box.texts(); len(got) != 1 || got[0] != "Carried over." || heard.UserID != "u1" {
		t.Fatalf("taken: %q %+v", got, heard)
	}
	host.DispatchInbound(context.Background(), hosted, firstMessage("what is the time"))
	if got := box.texts(); len(got) < 2 || got[len(got)-1] != "Here is your answer." {
		t.Errorf("anything else as before: %q", got)
	}
}
