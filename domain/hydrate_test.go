package domain_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func TestHydrateTool(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	tool := &domain.ToolDefinition{
		ID:            "tool_1",
		Name:          "echo",
		Description:   "Echoes input",
		IntegrationID: integration.ID,
		Action:        "mock.echo",
	}

	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	bound, err := domain.HydrateTool(tool, nil, integration, registry)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if bound.Spec.Name != "echo" {
		t.Fatalf("unexpected spec name: %s", bound.Spec.Name)
	}

	result, err := bound.Invoke(context.Background(), map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	got, ok := result.(map[string]any)
	if !ok || got["action"] != "mock.echo" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestHydrateTool_IntegrationMismatch(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	tool := &domain.ToolDefinition{ID: "tool_1", IntegrationID: "int_other"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	if _, err := domain.HydrateTool(tool, nil, integration, registry); err == nil {
		t.Fatal("expected an error for mismatched integration")
	}
}

func TestHydrateTool_NoExecutor(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "unregistered"}
	tool := &domain.ToolDefinition{ID: "tool_1", IntegrationID: integration.ID}
	registry := domain.NewExecutorRegistry()

	if _, err := domain.HydrateTool(tool, nil, integration, registry); err == nil {
		t.Fatal("expected an error for no registered executor")
	}
}

func TestHydrateSkillTools(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	toolA := &domain.ToolDefinition{ID: "tool_a", Name: "a", IntegrationID: integration.ID, Action: "mock.a"}
	toolB := &domain.ToolDefinition{ID: "tool_b", Name: "b", IntegrationID: integration.ID, Action: "mock.b"}

	skill := &domain.Skill{
		ID:    "skill_1",
		Tools: []domain.ToolBinding{{ToolID: toolA.ID}, {ToolID: toolB.ID}},
	}

	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})

	bound, err := domain.HydrateSkillTools(
		skill,
		nil,
		map[string]*domain.ToolDefinition{toolA.ID: toolA, toolB.ID: toolB},
		nil,
		map[string]*domain.Integration{integration.ID: integration},
		registry,
	)
	if err != nil {
		t.Fatalf("hydrate skill tools: %v", err)
	}
	if len(bound) != 2 || bound[0].Spec.Name != "a" || bound[1].Spec.Name != "b" {
		t.Fatalf("unexpected bound actions: %+v", bound)
	}
}

func TestHydrateSkillTools_UnknownTool(t *testing.T) {
	skill := &domain.Skill{ID: "skill_1", Tools: []domain.ToolBinding{{ToolID: "missing"}}}
	registry := domain.NewExecutorRegistry()

	if _, err := domain.HydrateSkillTools(skill, nil, nil, nil, nil, registry); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}

// ── a capability limited to some of a connection's abilities ──────────────────

func narrowFixture() (map[string]*domain.ToolDefinition, map[string]*domain.Integration, *domain.ExecutorRegistry) {
	gmail := &domain.Integration{ID: "gmail", Service: "mock"}
	cal := &domain.Integration{ID: "cal", Service: "mock"}
	tools := map[string]*domain.ToolDefinition{
		"g_list":  {ID: "g_list", Name: "list_messages", IntegrationID: "gmail", Action: "mock.a", ReadOnly: true},
		"g_send":  {ID: "g_send", Name: "send_email", IntegrationID: "gmail", Action: "mock.b"},
		"g_trash": {ID: "g_trash", Name: "trash_message", IntegrationID: "gmail", Action: "mock.c"},
		"c_list":  {ID: "c_list", Name: "list_events", IntegrationID: "cal", Action: "mock.d", ReadOnly: true},
		"c_del":   {ID: "c_del", Name: "delete_event", IntegrationID: "cal", Action: "mock.e"},
	}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})
	return tools, map[string]*domain.Integration{"gmail": gmail, "cal": cal}, registry
}

func namesOf(bound []*domain.BoundAction) map[string]bool {
	out := map[string]bool{}
	for _, b := range bound {
		out[b.Spec.Name] = true
	}
	return out
}

func TestHydrateSkillTools_ALimitedConnectionGivesOnlyTheNamedAbilities(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Integrations: []string{"gmail", "cal"}, LimitedIntegrations: []string{"gmail"}, Tools: []domain.ToolBinding{{ToolID: "g_list"}}}
	bound, err := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(bound)
	if !got["list_messages"] || got["send_email"] || got["trash_message"] {
		t.Errorf("Gmail is limited to what was named: %v", got)
	}
	if !got["list_events"] || !got["delete_event"] {
		t.Errorf("Calendar, with nothing named, is granted whole as before: %v", got)
	}
	if len(bound) != 3 {
		t.Errorf("each ability once: %d", len(bound))
	}
}

func TestHydrateSkillTools_LimitingOneConnectionDoesNotLimitAnother(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Integrations: []string{"gmail", "cal"}, LimitedIntegrations: []string{"gmail", "cal"}, Tools: []domain.ToolBinding{{ToolID: "c_list"}, {ToolID: "g_send"}}}
	bound, _ := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	got := namesOf(bound)
	if len(got) != 2 || !got["list_events"] || !got["send_email"] {
		t.Errorf("each limited to its own named abilities: %v", got)
	}
}

func TestHydrateSkillTools_AnAbilityNamedWithoutItsConnectionStillBindsAsBefore(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Tools: []domain.ToolBinding{{ToolID: "g_list"}}}
	bound, _ := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	if got := namesOf(bound); len(got) != 1 || !got["list_messages"] {
		t.Errorf("the old way is unchanged: %v", got)
	}
}

func TestHydrateSkillTools_ALimitedCapabilityNeverQuietlyWidens(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Integrations: []string{"gmail"}, LimitedIntegrations: []string{"gmail"}, Tools: []domain.ToolBinding{{ToolID: "g_list"}, {ToolID: "g_gone"}}}
	if _, err := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg); err == nil {
		t.Error("a named ability that no longer exists is an error, not a reason to grant the whole connection")
	}
	// the one named ability being switched off leaves the capability with nothing of that connection
	disabled := *tools["g_list"]
	disabled.Disabled = true
	tools["g_list"] = &disabled
	skill = &domain.Skill{ID: "s", Integrations: []string{"gmail"}, LimitedIntegrations: []string{"gmail"}, Tools: []domain.ToolBinding{{ToolID: "g_list"}}}
	bound, err := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	if err != nil || len(bound) != 0 {
		t.Errorf("switched off means nothing, not everything: %v %v", namesOf(bound), err)
	}
}

// How it always worked, and must keep working: naming an ability on top of a connection that is not limited adds it.
func TestHydrateSkillTools_NamedAbilitiesWithoutALimitAreAddedOnTopOfTheWholeConnection(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Integrations: []string{"gmail"}, Tools: []domain.ToolBinding{{ToolID: "g_list"}, {ToolID: "c_del"}}}
	bound, err := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	if err != nil {
		t.Fatal(err)
	}
	got := namesOf(bound)
	if len(got) != 4 || !got["list_messages"] || !got["send_email"] || !got["trash_message"] || !got["delete_event"] {
		t.Errorf("the whole of Gmail, plus the one named from Calendar: %v", got)
	}
}

func TestHydrateSkillTools_ALimitOnAConnectionNotListedGrantsNothing(t *testing.T) {
	tools, ints, reg := narrowFixture()
	skill := &domain.Skill{ID: "s", Integrations: []string{"gmail"}, LimitedIntegrations: []string{"cal"}}
	bound, _ := domain.HydrateSkillTools(skill, nil, tools, nil, ints, reg)
	if got := namesOf(bound); len(got) != 3 || got["list_events"] {
		t.Errorf("a limit does not add a connection, and does not narrow the one that is listed: %v", got)
	}
}

// askRecorder is an ApprovalRequester that records being asked and answers yes.
type askRecorder struct{ asked int }

func (a *askRecorder) RequestApproval(context.Context, domain.ConversationRef, string, int) (bool, error) {
	a.asked++
	return true, nil
}

// A person's own "ask me first" is for things that change something: it is honoured on a change,
// and never asked on a lookup, even if the setting was made earlier.
func TestUserApproval_IsAskedForAChangeAndNeverForALookup(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})
	run := func(readOnly bool) int {
		tool := &domain.ToolDefinition{ID: "t", Name: "thing", Description: "A thing", IntegrationID: integration.ID, Action: "mock.thing", ReadOnly: readOnly}
		bound, err := domain.HydrateTool(tool, nil, integration, registry)
		if err != nil {
			t.Fatal(err)
		}
		ask := &askRecorder{}
		ctx := domain.WithUserToolApprovals(context.Background(), map[string]bool{"t": true})
		ctx = domain.WithApprovalRequester(ctx, ask)
		ctx = domain.WithConversation(ctx, domain.ConversationRef{ChatID: "c"})
		if _, err := bound.Invoke(ctx, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		return ask.asked
	}
	if n := run(false); n != 1 {
		t.Errorf("a change the person asked to approve is asked once: %d", n)
	}
	if n := run(true); n != 0 {
		t.Errorf("a lookup is never asked: %d", n)
	}
}

// An unattended run has no one to ask. A change the person agreed to when they set it up runs without the per-use
// approval; any other change that asks first is refused, never run unapproved.
func TestUnattendedRun_AgreedChangeRunsWithoutAskingAndAnyOtherIsRefused(t *testing.T) {
	integration := &domain.Integration{ID: "int_1", Service: "mock"}
	registry := domain.NewExecutorRegistry()
	registry.Register("mock", echoExecutor{})
	tool := &domain.ToolDefinition{ID: "t", Name: "post_comment", Description: "Posts", IntegrationID: integration.ID, Action: "mock.post", RequiresApproval: true,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
	bound, err := domain.HydrateTool(tool, nil, integration, registry)
	if err != nil {
		t.Fatal(err)
	}
	attended := domain.WithConversation(context.Background(), domain.ConversationRef{ChatID: "c"})
	unattended := func(allowed ...string) context.Context { return domain.WithUnattendedAllowing(attended, allowed) }
	// ran reports whether the call reached the executor (the executor answers with its action).
	ran := func(ctx context.Context) bool {
		out, err := bound.Invoke(ctx, map[string]any{})
		m, ok := out.(map[string]any)
		return err == nil && ok && m["action"] == "mock.post"
	}
	if !ran(unattended("post_comment")) {
		t.Error("a change the person agreed to runs")
	}
	if ran(unattended("something_else")) {
		t.Error("a change that was not named is refused")
	}
	if ran(unattended()) {
		t.Error("with nothing allowed it is refused")
	}
	if ran(attended) {
		t.Error("an attended run is still asked (and there is no one to ask here)")
	}
}
