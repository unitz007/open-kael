package runtime_test

import (
	"context"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// oneSkillRouter always picks the same skill with a fixed intent, like Jev would.
type oneSkillRouter struct {
	skill  string
	intent domain.Intent
}

func (r oneSkillRouter) PickSkill(context.Context, string, []*domain.Skill) (string, float64, domain.Intent, error) {
	return r.skill, 1, r.intent, nil
}
func (r oneSkillRouter) PickSkills(context.Context, string, []*domain.Skill) ([]string, domain.Intent, error) {
	return []string{r.skill}, r.intent, nil
}

// recordingLLM remembers the tools the model was offered on every call.
type recordingLLM struct {
	mu   sync.Mutex
	seen [][]string
}

func (l *recordingLLM) Call(_ context.Context, _ []domain.Message, actions []domain.ActionSpec) (*domain.LLMResponse, error) {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.Name)
	}
	l.mu.Lock()
	l.seen = append(l.seen, names)
	l.mu.Unlock()
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "ok"}}}}, nil
}

func (l *recordingLLM) reset() {
	l.mu.Lock()
	l.seen = nil
	l.mu.Unlock()
}

func (l *recordingLLM) offered() map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for _, call := range l.seen {
		for _, n := range call {
			out[n] = true
		}
	}
	return out
}

// "What do you suggest" is a request for advice: the agent may look things up but
// must not act. Only tools marked read-only are offered then, so a look-up tool
// that nobody marked read-only is never consulted at all.
func TestRecommendIntent_OffersOnlyReadOnlyTools(t *testing.T) {
	run := func(t *testing.T, intent domain.Intent, tools ...*domain.ToolDefinition) map[string]bool {
		t.Helper()
		integration := &domain.Integration{ID: "int-fx", Service: "fx"}
		skill := &domain.Skill{ID: "s1", Name: "forex", AgentID: "a1", Integrations: []string{integration.ID}}
		llm := &recordingLLM{}
		agent := &domain.Agent{ID: "a1", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}}
		executors := domain.NewExecutorRegistry()
		executors.Register("slack", &fakeExecutor{})
		executors.Register("fx", &fakeExecutor{})
		toolsByID := map[string]*domain.ToolDefinition{}
		for _, td := range tools {
			td.IntegrationID = integration.ID
			toolsByID[td.ID] = td
		}
		deps := slackDeps(func(d *runtime.AgentDeps) {
			d.Executors = executors
			d.Memory = newMemMemory()
			d.ToolsByID = toolsByID
			d.IntegrationsByID[integration.ID] = integration
		})
		hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
		host := runtime.NewHost()
		host.SetSkillRouter(oneSkillRouter{skill: "forex", intent: intent})
		host.Register(hosted)
		if _, err := host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1"}, "I am looking at EURUSD, what do you suggest"); err != nil {
			t.Fatalf("handle turn: %v", err)
		}
		return llm.offered()
	}

	quote := func(readOnly bool) *domain.ToolDefinition {
		return &domain.ToolDefinition{ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Action: "q", ReadOnly: readOnly}
	}
	trade := &domain.ToolDefinition{ID: "t", Name: "place_trade", FunctionName: "place_trade", Action: "t", RequiresApproval: true, ApprovalTimeoutSeconds: 60}

	// Advice with a tool marked read-only: it can look things up, but never trade.
	got := run(t, domain.IntentRecommend, quote(true), trade)
	if !got["get_live_quote"] || got["place_trade"] {
		t.Fatalf("advice should offer the look-up and never the trade tool: %v", got)
	}

	// The same look-up with nothing saying it's read-only: the agent can't consult it.
	got = run(t, domain.IntentRecommend, quote(false), trade)
	if got["get_live_quote"] || got["place_trade"] {
		t.Fatalf("a tool not marked read-only must not be offered for advice: %v", got)
	}

	// Asking it to act offers everything.
	got = run(t, domain.IntentExecute, quote(false), trade)
	if !got["get_live_quote"] || !got["place_trade"] {
		t.Fatalf("an action request offers all tools: %v", got)
	}
}

// A question that is the kind of thing a learned skill is for may have that skill's changes done as its steps say: those
// changes (and no others) stay offered with the look-ups, for everyone's skills and the person's own, never someone
// else's.
func TestRecommendIntent_OffersTheChangesOfItsLearnedSkills(t *testing.T) {
	run := func(t *testing.T, user string, learned ...*domain.Skill) map[string]bool {
		t.Helper()
		integration := &domain.Integration{ID: "int-fx", Service: "fx"}
		skill := &domain.Skill{ID: "s1", Name: "forex", AgentID: "a1", Integrations: []string{integration.ID}}
		llm := &recordingLLM{}
		agent := &domain.Agent{ID: "a1", LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}, Learned: learned}
		executors := domain.NewExecutorRegistry()
		executors.Register("slack", &fakeExecutor{})
		executors.Register("fx", &fakeExecutor{})
		toolsByID := map[string]*domain.ToolDefinition{
			"q": {ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Action: "q", ReadOnly: true, IntegrationID: integration.ID},
			"n": {ID: "n", Name: "save_note", FunctionName: "save_note", Action: "n", IntegrationID: integration.ID},
			"t": {ID: "t", Name: "place_trade", FunctionName: "place_trade", Action: "t", RequiresApproval: true, ApprovalTimeoutSeconds: 60, IntegrationID: integration.ID},
		}
		deps := slackDeps(func(d *runtime.AgentDeps) {
			d.Executors = executors
			d.Memory = newMemMemory()
			d.ToolsByID = toolsByID
			d.IntegrationsByID[integration.ID] = integration
		})
		hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
		host := runtime.NewHost()
		host.SetSkillRouter(oneSkillRouter{skill: "forex", intent: domain.IntentRecommend})
		host.Register(hosted)
		if _, err := host.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1", UserID: user}, "how's EURUSD looking?"); err != nil {
			t.Fatalf("handle turn: %v", err)
		}
		return llm.offered()
	}
	check := &domain.Skill{ID: "l1", Origin: domain.SkillLearned, Steps: []domain.SkillStep{{Text: "Get the price", Ability: "q"}, {Text: "Note it down", Ability: "n"}}}
	if got := run(t, "p1", check); !got["get_live_quote"] || !got["save_note"] || got["place_trade"] {
		t.Errorf("everyone's skill's change, and no other: %v", got)
	}
	own := &domain.Skill{ID: "l2", Origin: domain.SkillLearned, PersonID: "p1", Steps: check.Steps}
	if got := run(t, "p1", own); !got["save_note"] {
		t.Errorf("the person's own: %v", got)
	}
	if got := run(t, "p2", own); got["save_note"] {
		t.Errorf("never someone else's: %v", got)
	}
	if got := run(t, "p1"); got["save_note"] || got["place_trade"] {
		t.Errorf("with nothing learned, look-ups only: %v", got)
	}
}
