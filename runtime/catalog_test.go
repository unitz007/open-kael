package runtime_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func catalogAgent(id, owner string, cat *domain.Catalog, llm domain.LLM, skill *domain.Skill, execs *domain.ExecutorRegistry) *runtime.HostedAgent {
	slack := &domain.Integration{ID: "int_slack", Service: "slack"}
	slackID := &domain.Identity{ID: "id_slack", IntegrationID: slack.ID}
	cat.PutIntegration(slack)
	cat.PutIdentity(slackID)
	return &runtime.HostedAgent{
		Agent: &domain.Agent{ID: id, CreatedBy: owner, LLMs: []domain.LLM{llm}, MaxIterations: 5, Skills: []*domain.Skill{skill}},
		Deps:  runtime.AgentDeps{Catalog: cat, Executors: execs, Memory: newMemMemory()},
	}
}

func fxSource(owner string) domain.StaticSource {
	return domain.StaticSource{
		Integ: &domain.Integration{ID: "custom_fx", Service: "fx", CreatedBy: owner},
		ToolDefs: []*domain.ToolDefinition{{
			ID: "custom_fx.get_quote", Name: "get_quote", FunctionName: "get_quote", IntegrationID: "custom_fx", Action: "q",
			Description: "Shows a price.", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject},
		}},
	}
}

// firstSkillRouter picks the agent's first skill every time, like the router would
// for a message that clearly fits it, so the skill's tools are offered directly.
type firstSkillRouter struct {
	host func(agentID string) *domain.Agent
}

func (r firstSkillRouter) PickSkill(_ context.Context, _ string, skills []*domain.Skill) (string, float64, domain.Intent, error) {
	return skills[0].Name, 1, domain.IntentExecute, nil
}
func (r firstSkillRouter) PickSkills(_ context.Context, _ string, skills []*domain.Skill) ([]string, domain.Intent, error) {
	return []string{skills[0].Name}, domain.IntentExecute, nil
}

func newRoutedHost() *runtime.Host {
	h := runtime.NewHost()
	h.SetSkillRouter(firstSkillRouter{})
	return h
}

func runTurn(t *testing.T, h *runtime.Host, hosted *runtime.HostedAgent) error {
	t.Helper()
	_, err := h.HandleTurn(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "C1"}, "hello there friend")
	return err
}

// A connection added while the server is running is available to the very next
// turn, with no restart and no second list to keep in step.
func TestCatalog_ToolAddedWhileRunningIsUsedByTheNextTurn(t *testing.T) {
	cat := domain.NewCatalog()
	execs := domain.NewExecutorRegistry()
	execs.Register("slack", &fakeExecutor{})
	execs.Register("fx", &fakeExecutor{})
	llm := &recordingLLM{}
	skill := &domain.Skill{ID: "s1", Name: "forex", AgentID: "a1", Integrations: []string{"custom_fx"}}
	hosted := catalogAgent("a1", "creator-a", cat, llm, skill, execs)
	host := newRoutedHost()
	host.Register(hosted)

	// Not registered yet: a skill granting it can't be loaded.
	if err := runTurn(t, host, hosted); err == nil {
		t.Fatal("a skill granting an integration that isn't in the catalog can't run")
	}
	if err := cat.AddSource(fxSource("creator-a")); err != nil {
		t.Fatal(err)
	}
	if err := runTurn(t, host, hosted); err != nil {
		t.Fatalf("after it is added: %v", err)
	}
	if !llm.offered()["get_quote"] {
		t.Fatalf("the tool added while running was not offered: %v", llm.offered())
	}

	// And removing it takes effect just as fast.
	cat.RemoveSource("custom_fx")
	llm.reset()
	if err := runTurn(t, host, hosted); err == nil {
		t.Fatal("a removed connection must stop being usable at once")
	}
	if llm.offered()["get_quote"] {
		t.Fatal("and its tool must not be offered")
	}
}

// One shared catalog, but an agent only ever sees what its own creator may use.
func TestCatalog_AnAgentNeverSeesAnotherCreatorsConnection(t *testing.T) {
	cat := domain.NewCatalog()
	execs := domain.NewExecutorRegistry()
	execs.Register("slack", &fakeExecutor{})
	execs.Register("fx", &fakeExecutor{})
	_ = cat.AddSource(fxSource("creator-a"))

	skill := func(agent string) *domain.Skill {
		return &domain.Skill{ID: "s-" + agent, Name: "forex", AgentID: agent, Integrations: []string{"custom_fx"}}
	}
	llmA, llmB := &recordingLLM{}, &recordingLLM{}
	a := catalogAgent("a1", "creator-a", cat, llmA, skill("a1"), execs)
	b := catalogAgent("b1", "creator-b", cat, llmB, skill("b1"), execs)
	host := newRoutedHost()
	host.Register(a)
	host.Register(b)

	if err := runTurn(t, host, a); err != nil || !llmA.offered()["get_quote"] {
		t.Fatalf("the owner's agent uses it: %v %v", err, llmA.offered())
	}
	if err := runTurn(t, host, b); err == nil {
		t.Fatal("another creator's agent must not be able to load a skill on it")
	}
	if llmB.offered()["get_quote"] {
		t.Fatal("another creator's agent was offered the tool")
	}

	// Platform integrations stay shared.
	_ = cat.AddSource(domain.StaticSource{
		Integ:    &domain.Integration{ID: "shared", Service: "fx"},
		ToolDefs: []*domain.ToolDefinition{{ID: "shared.look", Name: "look", IntegrationID: "shared", Action: "l", Description: "Looks.", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}},
	})
	shared := &domain.Skill{ID: "s-shared", Name: "shared", AgentID: "b2", Integrations: []string{"shared"}}
	llmC := &recordingLLM{}
	c := catalogAgent("b2", "creator-b", cat, llmC, shared, execs)
	host.Register(c)
	if err := runTurn(t, host, c); err != nil || !llmC.offered()["look"] {
		t.Fatalf("platform integrations are shared: %v %v", err, llmC.offered())
	}
}

// Agents built the old way (maps, no catalog) keep working.
func TestCatalog_AgentsBuiltFromMapsStillWork(t *testing.T) {
	tool := &domain.ToolDefinition{ID: "t", Name: "ping", IntegrationID: "int", Action: "p", ReadOnly: true, Description: "Pings."}
	llm := &recordingLLM{}
	execs := domain.NewExecutorRegistry()
	execs.Register("slack", &fakeExecutor{})
	execs.Register("svc", &fakeExecutor{})
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors, d.Memory = execs, newMemMemory()
		d.ToolsByID = map[string]*domain.ToolDefinition{"t": tool}
		d.IntegrationsByID["int"] = &domain.Integration{ID: "int", Service: "svc"}
	})
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1", LLMs: []domain.LLM{llm}, MaxIterations: 5,
		Skills: []*domain.Skill{{ID: "s", Name: "s", AgentID: "a1", Integrations: []string{"int"}}}}, Deps: deps}
	host := newRoutedHost()
	host.Register(hosted)
	if err := runTurn(t, host, hosted); err != nil || !llm.offered()["ping"] {
		t.Fatalf("%v %v", err, llm.offered())
	}
}
