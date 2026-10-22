package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// activityLog is a recorder that keeps what it was told.
type activityLog struct {
	mu       sync.Mutex
	people   []string // agent|person
	events   []string // agent|kind|detail
	messages []string // agent, one per message handled
}

func (a *activityLog) PersonActive(_ context.Context, agentID, person string) {
	a.mu.Lock()
	a.people = append(a.people, agentID+"|"+person)
	a.mu.Unlock()
}
func (a *activityLog) ActivityEvent(_ context.Context, agentID string, kind domain.ActivityKind, detail string) {
	a.mu.Lock()
	a.events = append(a.events, agentID+"|"+string(kind)+"|"+detail)
	a.mu.Unlock()
}
func (a *activityLog) MessageHandled(_ context.Context, agentID string) {
	a.mu.Lock()
	a.messages = append(a.messages, agentID)
	a.mu.Unlock()
}
func (a *activityLog) waitMessages(n int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		got := len(a.messages)
		a.mu.Unlock()
		if got >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.messages...)
}
func (a *activityLog) snapshot() ([]string, []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.people...), append([]string(nil), a.events...)
}

// waitEvents waits for the recorder (which is fed off the reply's path) to have n events.
func (a *activityLog) waitEvents(n int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ev := a.snapshot(); len(ev) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, ev := a.snapshot()
	return ev
}

func (a *activityLog) waitPeople(n int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := a.snapshot(); len(p) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, _ := a.snapshot()
	return p
}

// A person who writes to an agent is counted for that agent; what repeating tasks do on their
// own is not them being active.
func TestActivity_APersonWhoWritesIsCountedAndATaskRunningOnItsOwnIsNot(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"thanks!": domain.IntentChat}}
	r := newIntentRig(t, router)
	log := &activityLog{}
	r.host.SetActivityRecorder(log)

	r.say(t, "C1", "thanks!")
	if got := log.waitPeople(1); len(got) != 1 || got[0] != "a1|u1" {
		t.Fatalf("the person, for the agent: %q", got)
	}
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "check my calendar"); err != nil {
		t.Fatal(err)
	}
	waitTexts(r.box, 2)
	time.Sleep(100 * time.Millisecond)
	if got, _ := log.snapshot(); len(got) != 1 {
		t.Errorf("a repeating task is not the person being active: %q", got)
	}
}

// What the agent couldn't do is counted by a topic in a few plain words; the person's own words
// reach only the labeler, never the record.
func TestActivity_WhatTheAgentCouldNotDoIsKeptAsATopicNotAsTheWords(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"write me a poem about my sister Ada 0791234": domain.IntentOffTopic}}
	r := newIntentRig(t, router)
	log := &activityLog{}
	r.host.SetActivityRecorder(log)
	var asked []string
	var mu sync.Mutex
	r.host.SetTopicLabeler(func(_ context.Context, request string) string {
		mu.Lock()
		asked = append(asked, request)
		mu.Unlock()
		return "write a poem"
	})
	r.say(t, "C1", "write me a poem about my sister Ada 0791234")

	ev := log.waitEvents(1)
	if len(ev) != 1 || ev[0] != "a1|unsupported|write a poem" {
		t.Fatalf("a topic, not their words: %q", ev)
	}
	for _, e := range ev {
		if strings.Contains(e, "Ada") || strings.Contains(e, "0791234") {
			t.Errorf("the record must not hold their words: %q", e)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || !strings.Contains(asked[0], "poem") {
		t.Errorf("only the labeler sees the request: %q", asked)
	}
}

// With no labeler it is still counted, with no topic. A reply that already went out is never
// held up by the recording.
func TestActivity_WithoutALabelerItIsStillCountedAndRecordingNeverHoldsUpAReply(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"write me a poem": domain.IntentOffTopic}}
	r := newIntentRig(t, router)
	slow := make(chan struct{})
	r.host.SetActivityRecorder(&blockingActivity{release: slow})
	r.say(t, "C1", "write me a poem")
	if got := r.box.texts(); len(got) != 1 || got[0] != fixedDecline() {
		t.Fatalf("the reply is delivered even though recording is stuck: %q", got)
	}
	close(slow)

	r = newIntentRig(t, &intentRouter{intents: map[string]domain.Intent{"write me a poem": domain.IntentOffTopic}})
	log := &activityLog{}
	r.host.SetActivityRecorder(log)
	r.say(t, "C1", "write me a poem")
	if ev := log.waitEvents(1); len(ev) != 1 || ev[0] != "a1|unsupported|" {
		t.Errorf("counted, with no topic: %q", ev)
	}
}

type blockingActivity struct{ release chan struct{} }

func (b *blockingActivity) PersonActive(context.Context, string, string) { <-b.release }
func (b *blockingActivity) MessageHandled(context.Context, string)       { <-b.release }
func (b *blockingActivity) ActivityEvent(context.Context, string, domain.ActivityKind, string) {
	<-b.release
}

// A reply that didn't go through is counted with one of two plain reasons.
func TestActivity_AFailedReplyIsCountedWithAPlainReason(t *testing.T) {
	down := &funcLLM{call: func(int) (*domain.LLMResponse, error) { return nil, errors.New("openrouter 402: out of credits") }}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{down}, MaxIterations: 2}
	box := &outbox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	host := runtime.NewHost()
	hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })}
	host.Register(hosted)
	log := &activityLog{}
	host.SetActivityRecorder(log)

	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
	if _, err := host.HandleTurn(context.Background(), hosted, conv, "hello there"); err == nil {
		t.Fatal("the model is down: the turn fails")
	}
	ev := log.waitEvents(1)
	if len(ev) != 1 || ev[0] != "a1|failed|unavailable" {
		t.Fatalf("%q", ev)
	}
	if got := box.texts(); len(got) != 1 || strings.Contains(strings.ToLower(got[0]), "402") || strings.Contains(strings.ToLower(got[0]), "credit") {
		t.Errorf("the person is told plainly, no internals: %q", got)
	}
	// A repeating task failing is that person's business, not a failed reply.
	if err := host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "check my calendar"); err != nil {
		t.Fatal(err)
	}
	waitTexts(box, 2)
	time.Sleep(100 * time.Millisecond)
	if _, ev := log.snapshot(); len(ev) != 1 {
		t.Errorf("a task failing on its own is not counted: %q", ev)
	}
}

// Being asked to connect or sign in to a service is counted by the service, for the agent.
func TestActivity_ASignInRequestIsCountedByTheService(t *testing.T) {
	llm := finishOnlyLLM("Done: bought.")
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{llm}, MaxIterations: 5, IdentityIDs: []string{"id_aegis"}}
	host, hosted, _ := onboardingRig(t, agent)
	host.SetSetupChecker(func(context.Context, string, *domain.Agent) ([]string, error) { return []string{"id_aegis"}, nil })
	host.SetConnectURLGenerator(func(context.Context, string, string) (string, error) { return "https://app.example/connect", nil })
	host.SetConnectIntegrationNameLoader(func(context.Context, string) (string, error) { return "Aegis", nil })
	log := &activityLog{}
	host.SetActivityRecorder(log)

	host.DispatchInbound(context.Background(), hosted, firstMessage("buy 10000 EURUSD"))
	log.waitEvents(1)
	time.Sleep(150 * time.Millisecond) // anything extra would arrive by now
	if _, ev := log.snapshot(); len(ev) != 1 || ev[0] != "a1|signin|Aegis" {
		t.Fatalf("one request, one count: %q", ev)
	}
}

// Every message a person sends is counted for the agent, once, as a number and nothing more. What
// repeating tasks do on their own, and people not signed in, are not messages.
func TestActivity_EachMessageIsCountedOnceAndTasksAreNot(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"thanks!": domain.IntentChat, "cheers!": domain.IntentChat}}
	r := newIntentRig(t, router)
	log := &activityLog{}
	r.host.SetActivityRecorder(log)

	r.say(t, "C1", "thanks!")
	r.say(t, "C1", "cheers!")
	if got := log.waitMessages(2); len(got) != 2 || got[0] != "a1" || got[1] != "a1" {
		t.Fatalf("two messages, two counts, for the agent: %q", got)
	}
	// Someone who has not signed in is not a person to count.
	anon := domain.ConversationRef{Provider: "slack", ChatID: "C9", IdentityID: "id_slack", SenderID: "C9"}
	if _, err := r.host.HandleTurn(context.Background(), r.hosted, anon, "thanks!"); err != nil {
		t.Fatal(err)
	}
	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "check my calendar"); err != nil {
		t.Fatal(err)
	}
	waitTexts(r.box, 4)
	time.Sleep(100 * time.Millisecond)
	log.mu.Lock()
	n := len(log.messages)
	log.mu.Unlock()
	if n != 2 {
		t.Errorf("a repeating task is not a message: %d", n)
	}
}

// When the answering service can't answer, the person is told plainly it is temporarily unavailable
// (never a reason they can't act on, never "error"), and the creator's count says why.
func TestActivity_AnOutOfCreditOrUnavailableServiceIsSaidPlainlyToThePersonAndCountedWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		err    error
		detail string
	}{
		{fmt.Errorf("provider said: out of credits: %w", domain.ErrLLMOutOfCredit), "a1|failed|credit"},
		{fmt.Errorf("provider overloaded: %w", domain.ErrLLMUnavailable), "a1|failed|unavailable"},
	} {
		down := &funcLLM{call: func(int) (*domain.LLMResponse, error) { return nil, tc.err }}
		agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{down}, MaxIterations: 2}
		box := &outbox{}
		executors := domain.NewExecutorRegistry()
		executors.Register("slack", box)
		host := runtime.NewHost()
		hosted := &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })}
		host.Register(hosted)
		log := &activityLog{}
		host.SetActivityRecorder(log)

		conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
		start := time.Now()
		if _, err := host.HandleTurn(context.Background(), hosted, conv, "hello there"); err == nil {
			t.Fatal("the service is down: the turn fails")
		}
		if ev := log.waitEvents(1); len(ev) != 1 || ev[0] != tc.detail {
			t.Errorf("counted with its reason: %q, want %q", ev, tc.detail)
		}
		got := box.texts()
		if len(got) != 1 || got[0] != "I'm temporarily unavailable right now. Please try again in a little while." {
			t.Errorf("told plainly: %q", got)
		}
		lower := strings.ToLower(strings.Join(got, " "))
		for _, banned := range []string{"credit", "error", "402", "openrouter", "provider", "sorry"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%q must not reach the person: %q", banned, got)
			}
		}
		if tc.detail == "a1|failed|credit" && time.Since(start) > 900*time.Millisecond {
			t.Errorf("out of credit fails at once, not after retries: %v", time.Since(start))
		}
	}
}

// useLog is a recorder that also counts what each message was handled with.
type useLog struct {
	activityLog
	uses []string // agent|ids joined by ","; "" = with none
}

func (u *useLog) CapabilitiesUsed(_ context.Context, agentID string, skillIDs []string) {
	u.mu.Lock()
	u.uses = append(u.uses, agentID+"|"+strings.Join(skillIDs, ","))
	u.mu.Unlock()
}

func (u *useLog) waitUses(n int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for {
		u.mu.Lock()
		got := append([]string(nil), u.uses...)
		u.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Each message a person sends is counted against the capability it was handled with (by id, not name), or against none
// when the agent only talked or answered from what it knows. An alert or repeating task is not someone using it.
func TestActivity_EachMessageCountsWhatItWasHandledWith(t *testing.T) {
	router := &intentRouter{
		intents:      map[string]domain.Intent{"thanks!": domain.IntentChat, "Check audusd": domain.IntentRecommend, "Is the market open?": domain.IntentRecommend},
		followRouter: followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}},
	}
	r := newIntentRig(t, router)
	log := &useLog{}
	r.host.SetActivityRecorder(log)

	r.say(t, "C1", "Check audusd")
	r.say(t, "C2", "thanks!")
	r.say(t, "C3", "Is the market open?")
	got := log.waitUses(3)
	want := map[string]bool{"a1|s1": true, "a1|": true}
	if len(got) != 3 {
		t.Fatalf("one count per message: %q", got)
	}
	none := 0
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected: %q", g)
		}
		if g == "a1|" {
			none++
		}
	}
	if none != 2 {
		t.Errorf("talking and answering from what it knows count as handled with none: %q", got)
	}

	if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "Check audusd"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if after := log.waitUses(0); len(after) != 3 {
		t.Errorf("a repeating task is not a person using it: %q", after)
	}
}

// A recorder that does not count uses is fine: nothing is counted and nothing breaks.
func TestActivity_ARecorderThatDoesNotCountUsesIsLeftAlone(t *testing.T) {
	router := &intentRouter{intents: map[string]domain.Intent{"thanks!": domain.IntentChat}}
	r := newIntentRig(t, router)
	log := &activityLog{}
	r.host.SetActivityRecorder(log)
	r.say(t, "C1", "thanks!")
	if got := log.waitMessages(1); len(got) != 1 {
		t.Fatalf("the message is still counted: %q", got)
	}
}
