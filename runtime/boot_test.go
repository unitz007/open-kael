package runtime_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// TestBoot_WiresDelegationAndTurnHandling proves Boot produces both a
// working AgentRegistry (agent B can delegate to agent A's public Skill)
// and a working Host (HandleTurn actually runs and delivers), from
// nothing but the raw BootInput shape a loader like postgres.Store.LoadAll
// would produce.
func TestBoot_WiresDelegationAndTurnHandling(t *testing.T) {
	// Agent A: one public Skill, single tool, a plain "greet" action.
	greetTool := &domain.ToolDefinition{ID: "tool_greet", Name: "greet_tool", IntegrationID: "int_local", Action: "greet", Description: "Greets someone.", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	greetSkill := &domain.Skill{
		ID: "skill_greet", AgentID: "agent_a", Name: "greet",
		Description: "Greets someone by name",
		Visibility:  domain.SkillPublic,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{
			"name": {Type: domain.SchemaTypeString},
		}},
		Tools: []domain.ToolBinding{{ToolID: greetTool.ID}},
	}
	agentA := &domain.Agent{ID: "agent_a", Name: "Greeter", Skills: []*domain.Skill{greetSkill}}

	// Agent B: no tools of its own — its whole turn is answered by
	// calling finish directly, proving Host.HandleTurn works even for an
	// agent with nothing but delegate targets.
	agentB := &domain.Agent{ID: "agent_b", Name: "Delegator", MaxIterations: 5}

	localIntegration := &domain.Integration{ID: "int_local", Service: "local"}
	slackIntegration := &domain.Integration{ID: "int_slack", Service: "slack"}
	// Identity to connect conv.Provider="slack" to the slack integration for delivery.
	slackIdentity := &domain.Identity{ID: "id_slack", IntegrationID: slackIntegration.ID}

	executors := domain.NewExecutorRegistry()
	greetExec := &fakeExecutor{}
	executors.Register("local", greetExec)
	slackExec := &fakeExecutor{}
	executors.Register("slack", slackExec)

	input := runtime.BootInput{
		Agents:           []*domain.Agent{agentA, agentB},
		ToolsByID:        map[string]*domain.ToolDefinition{greetTool.ID: greetTool},
		IdentitiesByID:   map[string]*domain.Identity{slackIdentity.ID: slackIdentity},
		IntegrationsByID: map[string]*domain.Integration{localIntegration.ID: localIntegration, slackIntegration.ID: slackIntegration},
	}
	live := runtime.BootLive{
		Executors: executors,
		LLMs:      map[string][]domain.LLM{agentB.ID: {finishOnlyLLM("done")}}, // agentA never runs its own loop in this test
		Memory:    map[string]domain.Memory{},
	}

	registry, host := runtime.Boot(input, live)

	// AgentRegistry: agent B's directory should see agent A's public skill.
	delegates := registry.DirectoryFor(agentB.ID).PublicSkills(context.Background())
	found := false
	for _, d := range delegates {
		if d.Spec.Name == greetSkill.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected agent B's directory to include agent A's public skill %q, got %+v", greetSkill.Name, delegates)
	}

	// Host: agent B can actually run a turn and get a delivered reply.
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1"}
	hostedB, ok := host.Get(agentB.ID)
	if !ok {
		t.Fatal("expected agent B to be registered on the host")
	}
	result, err := host.HandleTurn(context.Background(), hostedB, conv, "hi")
	if err != nil {
		t.Fatalf("handle turn: %v", err)
	}
	if result.Status != domain.LoopStatusComplete {
		t.Fatalf("expected complete, got %s", result.Status)
	}
	if len(slackExec.calls) != 1 || slackExec.calls[0].action != domain.ActionSendMessage {
		t.Fatalf("expected the turn's reply to be delivered via the slack executor, got %+v", slackExec.calls)
	}
}

// Whatever is stored is held to the tool contract on the way in: a badly described
// tool is left out (and logged), the rest of its integration still loads, and every
// agent shares the one catalog.
func TestBoot_HoldsStoredToolsToTheContractAndSharesOneCatalog(t *testing.T) {
	good := &domain.ToolDefinition{ID: "t_good", Name: "good", IntegrationID: "int_a", Action: "g", Description: "Fine.", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	bad := &domain.ToolDefinition{ID: "t_bad", Name: "bad", IntegrationID: "int_a", Action: "b"} // no description, no schema
	execs := domain.NewExecutorRegistry()
	execs.Register("a", &fakeExecutor{})

	a1 := &domain.Agent{ID: "a1", MaxIterations: 3}
	a2 := &domain.Agent{ID: "a2", MaxIterations: 3}
	registry, host := runtime.Boot(runtime.BootInput{
		Agents:           []*domain.Agent{a1, a2},
		ToolsByID:        map[string]*domain.ToolDefinition{"t_good": good, "t_bad": bad},
		IdentitiesByID:   map[string]*domain.Identity{"id_a": {ID: "id_a", IntegrationID: "int_a"}, "id_orphan": {ID: "id_orphan", IntegrationID: "gone"}},
		IntegrationsByID: map[string]*domain.Integration{"int_a": {ID: "int_a", Service: "a"}},
	}, runtime.BootLive{Executors: execs})

	cat := registry.Catalog()
	if _, ok := cat.Tool("t_good"); !ok {
		t.Fatal("the well described tool loads")
	}
	if _, ok := cat.Tool("t_bad"); ok {
		t.Fatal("a tool that breaks the contract must not load")
	}
	if _, ok := cat.Identity("id_a"); !ok {
		t.Fatal("identities load with their integration")
	}
	if _, ok := cat.Identity("id_orphan"); !ok {
		t.Fatal("an identity whose integration is unknown is kept")
	}

	// Both agents see what is added to the catalog later, because it is the same catalog.
	if err := cat.AddSource(domain.StaticSource{
		Integ:    &domain.Integration{ID: "int_late", Service: "a"},
		ToolDefs: []*domain.ToolDefinition{{ID: "t_late", Name: "late", IntegrationID: "int_late", Action: "l", Description: "Later.", InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a1", "a2"} {
		hosted, ok := host.Get(id)
		if !ok {
			t.Fatalf("agent %s not hosted", id)
		}
		if hosted.Deps.Catalog != cat {
			t.Errorf("agent %s has a catalog of its own", id)
		}
	}
}
