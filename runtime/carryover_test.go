package runtime_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// followRouter answers each message from a script: the abilities it matches ("" key = none).
type followRouter struct {
	byText map[string][]string
	seen   []domain.Earlier
}

func (r *followRouter) PickSkill(context.Context, string, []*domain.Skill) (string, float64, domain.Intent, error) {
	return "", 0, domain.IntentRecommend, nil
}
func (r *followRouter) PickSkills(_ context.Context, text string, _ []*domain.Skill) ([]string, domain.Intent, error) {
	return r.byText[text], domain.IntentRecommend, nil
}
func (r *followRouter) PickSkillsInContext(_ context.Context, text string, e domain.Earlier, _ []*domain.Skill) ([]string, domain.Intent, error) {
	r.seen = append(r.seen, e)
	return r.byText[text], domain.IntentRecommend, nil
}

// toolSpy records the names of the actions the model was offered on each call.
type toolSpy struct {
	mu      sync.Mutex
	offered [][]string
}

func (s *toolSpy) Call(_ context.Context, _ []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	var names []string
	for _, a := range actions {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	s.mu.Lock()
	s.offered = append(s.offered, names)
	s.mu.Unlock()
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
}
func (s *toolSpy) calls() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.offered) }

type followRig struct {
	host   *runtime.Host
	hosted *runtime.HostedAgent
	box    *outbox
	spy    *toolSpy
	mem    *memMemory
}

func newFollowRig(t *testing.T, router runtime.SkillRouter) *followRig {
	t.Helper()
	spy := &toolSpy{}
	box := &outbox{}
	mem := newMemMemory()
	integ := &domain.Integration{ID: "int-fx", Service: "fx"}
	skill := &domain.Skill{ID: "s1", Name: "analyse_pair", Description: "Analyses a currency pair", AgentID: "a1", Integrations: []string{integ.ID}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{spy}, MaxIterations: 5, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	executors.Register("fx", &fakeExecutor{})
	quote := &domain.ToolDefinition{ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Action: "q", ReadOnly: true, IntegrationID: integ.ID}
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = mem
		d.ToolsByID = map[string]*domain.ToolDefinition{"q": quote}
		d.IntegrationsByID[integ.ID] = integ
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.SetSkillRouter(router)
	host.Register(hosted)
	return &followRig{host: host, hosted: hosted, box: box, spy: spy, mem: mem}
}

func (r *followRig) say(t *testing.T, chat, text string) {
	t.Helper()
	conv := domain.ConversationRef{Provider: "slack", ChatID: chat, IdentityID: "id_slack", UserID: "u1"}
	if _, err := r.host.HandleTurn(context.Background(), r.hosted, conv, text); err != nil {
		t.Fatalf("turn %q: %v", text, err)
	}
}

// The incident: "Pull up the full context" right after an analysis matched nothing, so the
// agent had no tools and promised work it could not do. A short follow-up now stays with
// the ability the conversation was just using.
func TestFollowUp_AShortMessageStaysWithWhatTheConversationWasDoing(t *testing.T) {
	router := &followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}}
	r := newFollowRig(t, router)

	r.say(t, "C1", "Check audusd")
	r.say(t, "C1", "Pull up the full context") // matches nothing on its own

	if r.spy.calls() != 2 {
		t.Fatalf("the follow-up must reach the agent with its tools, not a decline: %d model calls", r.spy.calls())
	}
	offered := strings.Join(r.spy.offered[1], ",")
	if !strings.Contains(offered, "get_live_quote") {
		t.Errorf("the follow-up must be offered the same ability's tools: %v", r.spy.offered[1])
	}
	// The router was shown what the follow-up follows: the earlier message and the reply.
	if len(router.seen) == 0 || router.seen[len(router.seen)-1].User != "Check audusd" || router.seen[len(router.seen)-1].Assistant != "ok" {
		t.Errorf("the router must see the person's earlier message and the reply: %+v", router.seen)
	}
}

func TestFollowUp_ALongOrUnrelatedMessageDoesNotInheritIt(t *testing.T) {
	long := "Separately, could you explain how mortgage interest is calculated on a repayment loan please"
	router := &followRouter{byText: map[string][]string{"Check audusd": {"analyse_pair"}}}
	r := newFollowRig(t, router)
	r.say(t, "C1", "Check audusd")
	r.say(t, "C1", long)                       // too long to be a follow-up
	r.say(t, "C2", "Pull up the full context") // a different chat has no earlier ability
	if r.spy.calls() != 1 {
		t.Fatalf("only the first message reaches the model; the others are declined: %d calls", r.spy.calls())
	}
}

// Nothing matches and there is nothing to carry over: the person is told, in fixed words,
// what the agent can do. No model writes it, so it cannot promise anything.
func TestOutOfScope_GetsAFixedReplyAndNoModelCall(t *testing.T) {
	r := newFollowRig(t, &followRouter{})
	r.say(t, "C1", "what's the weather like")

	want := "I can't help with that one. Here's what I can do:\n• Analyses a currency pair\n\nWhat would you like to try?"
	if got := r.box.texts(); len(got) != 1 || got[0] != want {
		t.Fatalf("fixed decline:\n got %q\nwant %q", got, want)
	}
	if r.spy.calls() != 0 {
		t.Fatalf("no model is asked to write a decline: %d calls", r.spy.calls())
	}
	// It is kept in the conversation like any reply, with no instruction text left behind.
	var stored []domain.Message
	for _, m := range r.mem.store {
		stored = m
	}
	if len(stored) != 2 || stored[0].Role != domain.RoleUser || stored[1].Role != domain.RoleAssistant || stored[1].Content != want {
		t.Fatalf("history must be the person's message and the fixed reply: %+v", stored)
	}
	for _, m := range stored {
		if m.Role == domain.RoleSystem {
			t.Errorf("no internal instruction may be left in the history: %q", m.Content)
		}
	}
}

// ── schedules: the reply is kept, and a failed run never shows its internals ──

type scheduleRouter struct{}

func (scheduleRouter) PickSkill(context.Context, string, []*domain.Skill) (string, float64, domain.Intent, error) {
	return "analyse_pair", 1, domain.IntentSchedule, nil
}
func (scheduleRouter) PickSkills(context.Context, string, []*domain.Skill) ([]string, domain.Intent, error) {
	return []string{"analyse_pair"}, domain.IntentSchedule, nil
}

// An intent handler answers for itself; the exchange must still be kept in the conversation,
// so "change it to 9am" has something to refer to. It must be delivered exactly once.
func TestIntentHandler_ItsReplyIsKeptInTheConversationAndSentOnce(t *testing.T) {
	r := newFollowRig(t, scheduleRouter{})
	r.host.SetIntentHandler(domain.IntentSchedule, func(_ context.Context, _, _, skillName, _, _, _ string) (string, error) {
		return "Done! Every day at 8:00 AM I'll run this for you: " + skillName, nil
	})
	r.say(t, "C1", "check my calendar every morning at 8")

	if got := r.box.texts(); len(got) != 1 {
		t.Fatalf("delivered once: %q", got)
	}
	var stored []domain.Message
	for _, m := range r.mem.store {
		stored = m
	}
	if len(stored) != 2 || stored[0].Role != domain.RoleUser || stored[1].Role != domain.RoleAssistant || !strings.HasPrefix(stored[1].Content, "Done! Every day at 8:00 AM") {
		t.Fatalf("the exchange must be kept: %+v", stored)
	}
	if r.spy.calls() != 0 {
		t.Errorf("the handler answers; no model call: %d", r.spy.calls())
	}
}

func TestScheduledRun_AFailureSaysWhatItWasInPlainWordsAndNothingElse(t *testing.T) {
	integ := &domain.Integration{ID: "int-tool", Service: "tool"}
	t1 := &domain.ToolDefinition{ID: "t1", Name: "one", IntegrationID: integ.ID, Action: "one"}
	skill := &domain.Skill{ID: "s1", Name: "morning_summary", Description: "Summarises my day", AgentID: "a1", Tools: []domain.ToolBinding{{ToolID: "t1"}}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", Skills: []*domain.Skill{skill}}
	box := &outbox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", failingExecutor{})
	executors.Register("slack", box)
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.ToolsByID = map[string]*domain.ToolDefinition{"t1": t1}
		d.IntegrationsByID[integ.ID] = integ
	})
	host := runtime.NewHost()
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: deps})
	if err := host.RunScheduledSkillForUser(context.Background(), "a1", "morning_summary", "u1", "id_slack", "C1"); err != nil {
		t.Fatal(err)
	}
	got := waitTexts(box, 1)
	want := "I couldn't finish your scheduled task just now (Summarises my day). I'll try again next time."
	if len(got) != 1 || got[0] != want {
		t.Fatalf("failure message:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got[0], "_") || strings.Contains(strings.ToLower(got[0]), "secret-detail") {
		t.Errorf("no internal name and no error text: %q", got[0])
	}
}

type failingExecutor struct{}

func (failingExecutor) Execute(context.Context, *domain.Identity, string, string, map[string]any) (any, error) {
	return nil, errors.New("secret-detail: connection refused to 10.0.0.1")
}

// A message that is only being friendly (hello, thanks, goodbye) is its own kind of intent:
// the model answers warmly in the person's language with no tools, whatever abilities the
// words happened to resemble, and the instruction that shaped the reply is not kept.
type chatRouter struct{}

func (chatRouter) PickSkill(context.Context, string, []*domain.Skill) (string, float64, domain.Intent, error) {
	return "analyse_pair", 1, domain.IntentChat, nil
}
func (chatRouter) PickSkills(context.Context, string, []*domain.Skill) ([]string, domain.Intent, error) {
	return []string{"analyse_pair"}, domain.IntentChat, nil // even if an ability scored well
}

func TestChat_ASocialMessageIsAnsweredWithoutToolsAndWithoutLeavingTheInstructionBehind(t *testing.T) {
	r := newFollowRig(t, chatRouter{})
	r.say(t, "C1", "thanks!")

	if r.spy.calls() != 1 {
		t.Fatalf("the model answers once: %d calls", r.spy.calls())
	}
	if got := strings.Join(r.spy.offered[0], ","); got != domain.FinishActionName {
		t.Errorf("a friendly message gets no tools, only the ability to reply: %q", got)
	}
	if got := r.box.texts(); len(got) != 1 || got[0] != "ok" {
		t.Errorf("the model's own reply is delivered once: %q", got)
	}
	var stored []domain.Message
	for _, m := range r.mem.store {
		stored = m
	}
	if len(stored) == 0 || stored[0].Role != domain.RoleUser || stored[0].Content != "thanks!" {
		t.Fatalf("history starts with what the person said: %+v", stored)
	}
	var replied bool
	for _, m := range stored {
		if m.Role == domain.RoleSystem {
			t.Errorf("the instruction that shaped the reply must not be stored: %q", m.Content)
		}
		for _, c := range m.ToolCalls {
			if c.Name == domain.FinishActionName && c.Arguments["content"] == "ok" {
				replied = true
			}
		}
	}
	if !replied {
		t.Errorf("the reply itself is kept: %+v", stored)
	}
}
