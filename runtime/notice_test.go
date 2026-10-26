package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// What people are told about who runs an agent goes into the welcome for someone new, and on its own before the answer
// for someone already chatting; the provider says it once per person.
func TestNotice_InTheWelcomeForSomeoneNewElseBeforeTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		onboarded  bool
		want       []string
		wantSecond []string
	}{
		{"new", false, []string{"Welcome to the desk!\n\nDesk is run by Ada.", "Here is your answer."}, []string{"Here is your answer."}},
		{"already chatting", true, []string{"Desk is run by Ada.", "Here is your answer."}, []string{"Here is your answer."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &domain.Agent{ID: "a1", Name: "Desk", Greeting: "Welcome to the desk!", LLMs: []domain.LLM{finishOnlyLLM("Here is your answer.")}, MaxIterations: 5}
			host, hosted, box := onboardingRig(t, agent)
			done := tc.onboarded
			host.SetOnboardingFlow(
				func(context.Context, string, string, string) (bool, error) { return done, nil },
				func(context.Context, string, string, string, string, string) error { done = true; return nil },
			)
			told := map[string]bool{}
			var seen domain.ConversationRef
			host.SetNotice(func(_ context.Context, a *domain.Agent, conv domain.ConversationRef) string {
				seen = conv
				if told[a.ID+conv.UserID] {
					return ""
				}
				told[a.ID+conv.UserID] = true
				return "Desk is run by Ada."
			})

			host.DispatchInbound(context.Background(), hosted, firstMessage("what is the time"))
			if got := box.texts(); len(got) != len(tc.want) || got[0] != tc.want[0] || got[len(got)-1] != tc.want[len(tc.want)-1] {
				t.Fatalf("first message: %q", got)
			}
			if seen.UserID != "u1" || seen.IdentityID != "id_slack" {
				t.Errorf("the provider knows who and where: %+v", seen)
			}
			before := len(box.texts())
			host.DispatchInbound(context.Background(), hosted, firstMessage("and again"))
			if got := box.texts()[before:]; len(got) != 1 || got[0] != tc.wantSecond[0] {
				t.Errorf("told once only: %q", got)
			}
		})
	}
}

// personRecorder keeps what the person recorder is told.
type personRecorder struct {
	mu     sync.Mutex
	seen   []domain.ConversationRef
	events []domain.ActivityKind
}

func (r *personRecorder) PersonActive(context.Context, string, string)                       {}
func (r *personRecorder) ActivityEvent(context.Context, string, domain.ActivityKind, string) {}
func (r *personRecorder) MessageHandled(context.Context, string)                             {}
func (r *personRecorder) PersonSeen(_ context.Context, _ string, conv domain.ConversationRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, conv)
}
func (r *personRecorder) PersonEvent(_ context.Context, _ string, _ domain.ConversationRef, kind domain.ActivityKind, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kind)
}

// A recorder that wants to know who wrote is told the person and the chat app, with each message.
func TestPersonRecorder_IsToldWhoWroteAndWhere(t *testing.T) {
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{finishOnlyLLM("ok")}, MaxIterations: 5}
	host, hosted, _ := onboardingRig(t, agent)
	rec := &personRecorder{}
	host.SetActivityRecorder(rec)
	host.DispatchInbound(context.Background(), hosted, firstMessage("hello there"))
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec.mu.Lock()
		n := len(rec.seen)
		rec.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) == 0 || rec.seen[0].UserID != "u1" || rec.seen[0].IdentityID != "id_slack" {
		t.Fatalf("told who and where: %+v", rec.seen)
	}
}
