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
	greetTool := &domain.ToolDefinition{ID: "tool_greet", Name: "greet_tool", IntegrationID: "int_local", Action: "greet"}
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
