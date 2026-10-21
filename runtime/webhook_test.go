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

// A delivery whose payload doesn't match what its event promised is dropped: nothing is told of it.
func TestRegisterEventSources_DropsAPayloadThatBreaksTheEventsSchema(t *testing.T) {
	host := runtime.NewHost()
	told := make(chan string, 1)
	host.SetEventDispatcher(func(_ context.Context, name, _, _, _ string) { told <- name })
	src := &fakeEventSource{path: "/webhooks/github", eventName: "github.push", payload: `{"nonsense":true}`}
	sources := domain.NewEventSourceRegistry()
	sources.Register("id-gh", src, src.events())
	mux := http.NewServeMux()
	host.RegisterEventSources(mux, sources)

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}")))
	select {
	case name := <-told:
		t.Fatalf("%q was passed on although its facts break the schema", name)
	case <-time.After(150 * time.Millisecond):
	}
}

// Every event that arrives is told to the dispatcher the application registered, with the person as the provider names them.
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
