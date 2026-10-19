package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

type sendExec struct {
	result any
	err    error
	sent   []map[string]any
}

func (e *sendExec) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, input map[string]any) (any, error) {
	e.sent = append(e.sent, input)
	return e.result, e.err
}

func deliverRig(exec domain.Executor) (*Host, *HostedAgent, domain.ConversationRef) {
	execs := domain.NewExecutorRegistry()
	execs.Register("telegram", exec)
	hosted := &HostedAgent{Agent: &domain.Agent{ID: "a1"}, Deps: AgentDeps{
		IdentitiesByID:   map[string]*domain.Identity{"bot": {ID: "bot", IntegrationID: "tg"}},
		IntegrationsByID: map[string]*domain.Integration{"tg": {ID: "tg", Service: "telegram"}},
		Executors:        execs,
	}}
	return NewHost(), hosted, domain.ConversationRef{Provider: "telegram", ChatID: "chat1", IdentityID: "bot"}
}

func TestDeliverBestEffortID_ReturnsTheSentMessageID(t *testing.T) {
	exec := &sendExec{result: map[string]any{"message_id": "42", "thread_id": "42"}}
	h, hosted, conv := deliverRig(exec)
	if got := h.deliverBestEffortID(context.Background(), hosted, conv, "hello", map[string]any{"web_app_button": "x"}); got != "42" {
		t.Errorf("want message id 42, got %q", got)
	}
	if len(exec.sent) != 1 || exec.sent[0]["recipient"] != "chat1" || exec.sent[0]["web_app_button"] != "x" {
		t.Errorf("message not sent as expected: %v", exec.sent)
	}
}

func TestDeliverBestEffortID_EmptyWhenUnknownOrFailed(t *testing.T) {
	for name, exec := range map[string]*sendExec{
		"no id reported": {result: map[string]any{"ok": true}},
		"not a map":      {result: "sent"},
		"send failed":    {err: fmt.Errorf("blocked")},
	} {
		h, hosted, conv := deliverRig(exec)
		if got := h.deliverBestEffortID(context.Background(), hosted, conv, "hello"); got != "" {
			t.Errorf("%s: want empty id, got %q", name, got)
		}
	}
	h, hosted, _ := deliverRig(&sendExec{})
	if got := h.deliverBestEffortID(context.Background(), hosted, domain.ConversationRef{Provider: "slack", ChatID: "c"}, "x"); got != "" {
		t.Errorf("unknown provider: want empty id, got %q", got)
	}
}

// deliverBestEffort keeps its old contract: it delivers and returns nothing.
func TestDeliverBestEffort_StillDelivers(t *testing.T) {
	exec := &sendExec{result: map[string]any{"message_id": "7"}}
	h, hosted, conv := deliverRig(exec)
	h.deliverBestEffort(context.Background(), hosted, conv, "hi")
	if len(exec.sent) != 1 {
		t.Errorf("expected one send, got %d", len(exec.sent))
	}
}

func TestConnectPromptHook_ReportsTheSentPrompt(t *testing.T) {
	exec := &sendExec{result: map[string]any{"message_id": "99"}}
	h, hosted, conv := deliverRig(exec)
	h.connectURLGenerator = func(context.Context, string, string) (string, error) { return "https://app.example/connect?s=x", nil }
	var got []ConnectPrompt
	h.SetConnectPromptHook(func(_ context.Context, p ConnectPrompt) { got = append(got, p) })

	req := h.resolveConnectRequester(context.Background(), hosted, conv, "u1")
	if req == nil {
		t.Fatal("expected a requester")
	}
	ctx := domain.WithOriginalMessage(context.Background(), "show my inbox")
	if _, err := req.RequestConnect(ctx, conv, &domain.Identity{ID: "gmail-signin"}, "u1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("hook should fire once, fired %d times", len(got))
	}
	p := got[0]
	if p.UserID != "u1" || p.IdentityID != "gmail-signin" || p.MessageID != "99" || p.Conv.ChatID != "chat1" || p.Text == "" {
		t.Errorf("hook got %+v", p)
	}
	if btn, _ := exec.sent[0]["web_app_button"].(map[string]any); btn == nil || btn["url"] != "https://app.example/connect?s=x" {
		t.Errorf("the prompt should carry the Connect button: %v", exec.sent[0])
	}
}

func TestConnectPromptHook_NotCalledWithoutAMessageID(t *testing.T) {
	exec := &sendExec{result: map[string]any{"ok": true}} // messenger reports no id
	h, hosted, conv := deliverRig(exec)
	h.connectURLGenerator = func(context.Context, string, string) (string, error) { return "https://app.example/c", nil }
	called := false
	h.SetConnectPromptHook(func(context.Context, ConnectPrompt) { called = true })
	req := h.resolveConnectRequester(context.Background(), hosted, conv, "u1")
	_, _ = req.RequestConnect(context.Background(), conv, &domain.Identity{ID: "x"}, "u1")
	if called {
		t.Errorf("no message id means nothing to edit later, so the hook must not fire")
	}
}

// ── One connect prompt, not one per tool call ─────────────────────────────────

// lockedExec is a messenger that is safe to call from several goroutines.
type lockedExec struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (e *lockedExec) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, input map[string]any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, input)
	return map[string]any{"message_id": fmt.Sprint(len(e.sent))}, nil
}
func (e *lockedExec) count() int { e.mu.Lock(); defer e.mu.Unlock(); return len(e.sent) }

func promptRig(t *testing.T) (*Host, domain.ConnectRequester, *lockedExec, domain.ConversationRef, *time.Time) {
	t.Helper()
	exec := &lockedExec{}
	h, hosted, conv := deliverRig(exec)
	h.connectURLGenerator = func(context.Context, string, string) (string, error) { return "https://app.example/connect?s=x", nil }
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	old := connectPromptNow
	connectPromptNow = func() time.Time { return now }
	t.Cleanup(func() { connectPromptNow = old })
	return h, h.resolveConnectRequester(context.Background(), hosted, conv, "u1"), exec, conv, &now
}

// The model calls three tools at the same moment; the person gets ONE prompt.
func TestConnectPrompt_SeveralCallsAtOnceSendOnePrompt(t *testing.T) {
	_, req, exec, conv, _ := promptRig(t)
	ctx := domain.WithOriginalMessage(context.Background(), "I need the analysis on EURUSD")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := req.RequestConnect(ctx, conv, &domain.Identity{ID: "finos"}, "u1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := exec.count(); n != 1 {
		t.Fatalf("eight simultaneous calls should send exactly one prompt, sent %d", n)
	}
}

// The prompt is not repeated, but what the person asked is still remembered, so it
// can be picked up once they connect.
func TestConnectPrompt_TheRequestIsStillRememberedWhenThePromptIsNotRepeated(t *testing.T) {
	h, req, _, conv, _ := promptRig(t)
	id := &domain.Identity{ID: "finos"}
	req.RequestConnect(domain.WithOriginalMessage(context.Background(), "first question"), conv, id, "u1")  //nolint:errcheck
	req.RequestConnect(domain.WithOriginalMessage(context.Background(), "second question"), conv, id, "u1") //nolint:errcheck
	v, ok := h.pendingAuthTurns.Load("u1:finos")
	if !ok || v.(pendingAuthTurn).userMessage != "second question" {
		t.Errorf("the latest request should be the one replayed, got %+v", v)
	}
}

func TestConnectPrompt_DifferentPeopleAndConnectionsEachGetTheirOwn(t *testing.T) {
	_, req, exec, conv, _ := promptRig(t)
	ctx := domain.WithOriginalMessage(context.Background(), "q")
	req.RequestConnect(ctx, conv, &domain.Identity{ID: "finos"}, "u1") //nolint:errcheck
	req.RequestConnect(ctx, conv, &domain.Identity{ID: "gmail"}, "u1") //nolint:errcheck: another connection
	req.RequestConnect(ctx, conv, &domain.Identity{ID: "finos"}, "u2") //nolint:errcheck: another person
	if n := exec.count(); n != 3 {
		t.Errorf("a different connection or person must each be prompted, sent %d", n)
	}
}

// Someone who ignored the first prompt is asked again later.
func TestConnectPrompt_AsksAgainAfterTheCooldown(t *testing.T) {
	_, req, exec, conv, now := promptRig(t)
	ctx := domain.WithOriginalMessage(context.Background(), "q")
	id := &domain.Identity{ID: "finos"}
	req.RequestConnect(ctx, conv, id, "u1") //nolint:errcheck
	*now = now.Add(connectPromptCooldown - time.Second)
	req.RequestConnect(ctx, conv, id, "u1") //nolint:errcheck
	if exec.count() != 1 {
		t.Fatalf("inside the cooldown: still one prompt, got %d", exec.count())
	}
	*now = now.Add(2 * time.Second)
	req.RequestConnect(ctx, conv, id, "u1") //nolint:errcheck
	if exec.count() != 2 {
		t.Errorf("after the cooldown a fresh prompt is sent, got %d", exec.count())
	}
}

func TestConnectPrompt_TheRecordOfPastPromptsDoesNotGrowForever(t *testing.T) {
	h, _, _, _, now := promptRig(t)
	for i := 0; i < 500; i++ {
		h.shouldSendConnectPrompt(fmt.Sprintf("u%d:i", i))
	}
	*now = now.Add(2 * connectPromptCooldown)
	h.shouldSendConnectPrompt("late:i")
	if len(h.connectPromptSent) != 1 {
		t.Errorf("old entries should be forgotten, %d remain", len(h.connectPromptSent))
	}
}

// A tool that needs a connection also counts as a sign-in request for the agent, once per prompt.
func TestConnectRequester_CountsTheSignInRequestByService(t *testing.T) {
	exec := &sendExec{result: map[string]any{"message_id": "5"}}
	h, hosted, conv := deliverRig(exec)
	h.connectURLGenerator = func(context.Context, string, string) (string, error) { return "https://app.example/c", nil }
	h.SetConnectIntegrationNameLoader(func(context.Context, string) (string, error) { return "Google Calendar", nil })
	rec := &recordingActivity{}
	h.SetActivityRecorder(rec)

	req := h.resolveConnectRequester(context.Background(), hosted, conv, "u1")
	for i := 0; i < 3; i++ { // a model calling three tools at once sends one prompt
		_, _ = req.RequestConnect(context.Background(), conv, &domain.Identity{ID: "cal"}, "u1")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rec.count() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.all(); len(got) != 1 || got[0] != hosted.Agent.ID+"|signin|Google Calendar" {
		t.Errorf("one prompt, one count: %q", got)
	}
}

type recordingActivity struct {
	mu sync.Mutex
	ev []string
}

func (r *recordingActivity) PersonActive(context.Context, string, string) {}
func (r *recordingActivity) ActivityEvent(_ context.Context, agentID string, kind domain.ActivityKind, detail string) {
	r.mu.Lock()
	r.ev = append(r.ev, agentID+"|"+string(kind)+"|"+detail)
	r.mu.Unlock()
}
func (r *recordingActivity) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.ev) }
func (r *recordingActivity) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ev...)
}
