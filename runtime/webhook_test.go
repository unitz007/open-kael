package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// fakeEventSource is a minimal domain.EventSource — always verifies, always
// ingests to a fixed (eventName, payload), so tests focus on what the Host
// does with an event-triggered Skill's result, not signature schemes.
type fakeEventSource struct {
	path      string
	eventName string
	payload   string
}

func (s *fakeEventSource) Path() string                        { return s.path }
func (s *fakeEventSource) Verify(_ []byte, _ http.Header) bool { return true }

// events returns a single-event catalogue that always matches, returning the
// fixed payload from s. Pass to EventSourceRegistry.Register.
func (s *fakeEventSource) events() []*domain.IntegrationEvent {
	name, payload := s.eventName, s.payload
	return []*domain.IntegrationEvent{{
		Name:        name,
		Description: "Something happened",
		Arrival:     domain.ArrivalPushed,
		Subject:     domain.EventSubject{Kind: domain.SubjectNobody},
		Payload: domain.Schema{Type: domain.SchemaTypeObject, Required: []string{"number"}, Properties: map[string]domain.Schema{
			"number": {Type: domain.SchemaTypeInteger, Description: "Its number."},
		}},
		RepeatKey: "number",
		Sample:    map[string]any{"number": 1},
		Handler: func(_ []byte, _ http.Header) (string, string, bool, error) {
			return payload, "", true, nil
		},
	}}
}

// singleToolSkill builds a Skill bound to exactly one Tool, triggered by a
// named event, with an optional NotifyConversation.
func singleToolSkill(eventName string, notify *domain.ConversationRef) (*domain.Skill, *domain.ToolDefinition, *domain.Integration) {
	integration := &domain.Integration{ID: "int-tool", Service: "tool"}
	tool := &domain.ToolDefinition{
		ID:            "t1",
		Name:          "do_thing",
		Description:   "Does a thing",
		IntegrationID: integration.ID,
		Action:        "do_thing",
	}
	skill := &domain.Skill{
		ID:      "s1",
		Name:    "event_skill",
		AgentID: "a1",
		Tools:   []domain.ToolBinding{{ToolID: tool.ID}},
		Trigger: &domain.Trigger{Type: domain.TriggerTypeEvent, Value: eventName, NotifyConversation: notify},
	}
	return skill, tool, integration
}

func waitForCalls(t *testing.T, exec *fakeExecutor, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(exec.calls()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d executor call(s), got %d: %+v", n, len(exec.calls()), exec.calls())
}

func TestRegisterEventSources_NotifiesConversationOnSuccess(t *testing.T) {
	notify := &domain.ConversationRef{Provider: "teams", ChatID: "channel-1"}
	skill, tool, toolIntegration := singleToolSkill("github.pull_request.opened", notify)

	toolExec := &fakeExecutor{}
	teamsExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)
	executors.Register("teams", teamsExec)

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration, "int-teams": {ID: "int-teams", Service: "teams"}},
			IdentitiesByID:   map[string]*domain.Identity{"id-teams": {ID: "id-teams", IntegrationID: "int-teams"}},
			Executors:        executors,
		},
	}
	host := runtime.NewHost()
	host.Register(hosted)

	src := &fakeEventSource{
		path:      "/webhooks/github",
		eventName: "github.pull_request.opened",
		payload:   `{"action":"opened","number":1}`,
	}
	sources := domain.NewEventSourceRegistry()
	sources.Register("id-gh", src, src.events())

	mux := http.NewServeMux()
	host.RegisterEventSources(mux, sources)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}"))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	waitForCalls(t, toolExec, 1)
	if toolExec.calls()[0].input["trigger_input"] != `{"action":"opened","number":1}` {
		t.Fatalf("expected the tool to receive the event payload, got %+v", toolExec.calls()[0].input)
	}
	if toolExec.calls()[0].input["notify_conversation"] != *notify {
		t.Fatalf("expected the tool to see where its result will be posted, got %+v", toolExec.calls()[0].input)
	}

	waitForCalls(t, teamsExec, 1)
	if teamsExec.calls()[0].action != domain.ActionSendMessage {
		t.Fatalf("expected a send-message notification, got action %q", teamsExec.calls()[0].action)
	}
	if teamsExec.calls()[0].input["recipient"] != "channel-1" {
		t.Fatalf("expected notification to target the configured channel, got %+v", teamsExec.calls()[0].input)
	}
	if teamsExec.calls()[0].input["text"] != "ok" {
		t.Fatalf("expected the tool's result text forwarded verbatim, got %+v", teamsExec.calls()[0].input)
	}
}

func TestRegisterEventSources_NoNotifyConversation_NeverCallsDelivery(t *testing.T) {
	skill, tool, toolIntegration := singleToolSkill("github.push", nil)

	toolExec := &fakeExecutor{}
	teamsExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)
	executors.Register("teams", teamsExec)

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration, "int-teams": {ID: "int-teams", Service: "teams"}},
			Executors:        executors,
		},
	}
	host := runtime.NewHost()
	host.Register(hosted)

	sources := domain.NewEventSourceRegistry()
	fakeGH := &fakeEventSource{path: "/webhooks/github", eventName: "github.push", payload: `{"number":2}`}
	sources.Register("id-gh", fakeGH, fakeGH.events())

	mux := http.NewServeMux()
	host.RegisterEventSources(mux, sources)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}"))
	mux.ServeHTTP(rec, req)

	waitForCalls(t, toolExec, 1)
	time.Sleep(50 * time.Millisecond)
	if len(teamsExec.calls()) != 0 {
		t.Fatalf("expected no notification when Trigger.NotifyConversation is nil, got %+v", teamsExec.calls())
	}
}

func TestEmit_InternalEvent_TriggersSubscribedSkill(t *testing.T) {
	skill, tool, toolIntegration := singleToolSkill("user.channel.connected", nil)

	toolExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)

	agent := &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}}
	hosted := &runtime.HostedAgent{
		Agent: agent,
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration},
			Executors:        executors,
		},
	}
	host := runtime.NewHost()
	host.Register(hosted)

	// RegisterEventSources with an empty registry — just wire up subscriptions
	host.RegisterEventSources(http.NewServeMux(), domain.NewEventSourceRegistry())

	// Fire an internal event directly
	host.Emit(t.Context(), "user.channel.connected", `{"user_id":"u1","channel":"C123"}`)

	waitForCalls(t, toolExec, 1)
	if toolExec.calls()[0].input["trigger_input"] != `{"user_id":"u1","channel":"C123"}` {
		t.Fatalf("expected payload forwarded to tool, got %+v", toolExec.calls()[0].input)
	}
}

func TestRegisterEventSources_VerificationFailure_Returns401(t *testing.T) {
	source := &fakeEventSource{path: "/webhooks/github", eventName: "github.push", payload: `{"number":2}`}

	// Override Verify to reject
	type rejectingSource struct{ fakeEventSource }
	rejector := &struct {
		fakeEventSource
		verified bool
	}{fakeEventSource: *source}

	customSources := domain.NewEventSourceRegistry()
	customSources.Register("id-gh", &verifyFailSource{path: "/webhooks/github"}, nil)

	host := runtime.NewHost()
	mux := http.NewServeMux()
	host.RegisterEventSources(mux, customSources)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("payload"))
	mux.ServeHTTP(rec, req)
	_ = rejector

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on verification failure, got %d", rec.Code)
	}
}

// verifyFailSource always rejects the signature.
type verifyFailSource struct{ path string }

func (s *verifyFailSource) Path() string                        { return s.path }
func (s *verifyFailSource) Verify(_ []byte, _ http.Header) bool { return false }
func (s *verifyFailSource) Ingest(_ []byte, _ http.Header) (string, string, string, bool, error) {
	return "", "", "", false, nil
}

// A delivery whose payload doesn't match what its event promised is dropped: nothing reacts to it.
func TestRegisterEventSources_DropsAPayloadThatBreaksTheEventsSchema(t *testing.T) {
	skill, tool, toolIntegration := singleToolSkill("github.push", nil)
	toolExec := &fakeExecutor{}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", toolExec)
	hosted := &runtime.HostedAgent{
		Agent: &domain.Agent{ID: "a1", Skills: []*domain.Skill{skill}},
		Deps: runtime.AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{tool.ID: tool},
			IntegrationsByID: map[string]*domain.Integration{toolIntegration.ID: toolIntegration},
			Executors:        executors,
		},
	}
	host := runtime.NewHost()
	host.Register(hosted)

	src := &fakeEventSource{path: "/webhooks/github", eventName: "github.push", payload: `{"nonsense":true}`}
	sources := domain.NewEventSourceRegistry()
	sources.Register("id-gh", src, src.events())
	mux := http.NewServeMux()
	host.RegisterEventSources(mux, sources)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}")))
	time.Sleep(150 * time.Millisecond)
	if n := len(toolExec.calls()); n != 0 {
		t.Fatalf("the skill ran %d time(s) for a payload that breaks the schema", n)
	}
}

// Every event that arrives is also told to the dispatcher the application registered, whether or not a
// capability listens for it, with the person as the provider names them.
func TestRegisterEventSources_TellsTheDispatcherOfEveryEvent(t *testing.T) {
	host := runtime.NewHost()
	got := make(chan [4]string, 1)
	host.SetEventDispatcher(func(_ context.Context, name, payload, source, actor string) {
		got <- [4]string{name, payload, source, actor}
	})
	src := &fakeEventSource{path: "/webhooks/github", eventName: "github.push", payload: `{"number":3}`}
	sources := domain.NewEventSourceRegistry()
	events := src.events()
	handler := events[0].Handler
	events[0].Handler = func(b []byte, h http.Header) (string, string, bool, error) {
		p, _, ok, err := handler(b, h)
		return p, "octocat", ok, err
	}
	sources.Register("id-gh", src, events)
	mux := http.NewServeMux()
	host.RegisterEventSources(mux, sources)
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}")))
	select {
	case v := <-got:
		if v != [4]string{"github.push", `{"number":3}`, "id-gh", "octocat"} {
			t.Fatalf("%v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatcher was not told")
	}
}
