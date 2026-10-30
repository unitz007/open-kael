package runtime_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// editMemory keeps exchanges, as Postgres does.
type editMemory struct {
	*memMemory
	mu   sync.Mutex
	last map[string]domain.LastExchange
}

func exKey(k domain.ConversationKey) string { return k.AgentID + "|" + k.IdentityID + "|" + k.ChatID }

func (m *editMemory) LastExchange(_ context.Context, k domain.ConversationKey) (domain.LastExchange, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.last[exKey(k)]
	return e, ok
}
func (m *editMemory) SetLastExchange(_ context.Context, k domain.ConversationKey, e domain.LastExchange) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last[exKey(k)] = e
}
func (m *editMemory) RewindLastExchange(_ context.Context, k domain.ConversationKey) error {
	m.memMemory.mu.Lock()
	h := m.memMemory.store[k]
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == domain.RoleUser {
			m.memMemory.store[k] = h[:i]
			break
		}
	}
	m.memMemory.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.last, exKey(k))
	return nil
}

// replyBox is a chat app that numbers what it sends and can put a new reply in an old one's place.
type replyBox struct {
	mu       sync.Mutex
	sent     []string
	replaced []string
}

func (b *replyBox) Execute(_ context.Context, _ *domain.Identity, _, _ string, in map[string]any) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, in["text"].(string))
	return map[string]any{"message_id": fmt.Sprintf("r%d", len(b.sent))}, nil
}
func (b *replyBox) ReplaceReply(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, id, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.replaced = append(b.replaced, id+"="+text)
	return nil
}
func (b *replyBox) snapshot() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...), append([]string(nil), b.replaced...)
}

// echoLLM answers "about: <the person's words>" and keeps each system prompt; hold, when set, keeps its first call
// waiting until released (or its turn is stopped).
type echoLLM struct {
	mu      sync.Mutex
	prompts []string
	calls   int
	hold    chan struct{}
}

func (l *echoLLM) Call(ctx context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.prompts = append(l.prompts, msgs[0].Content)
	l.mu.Unlock()
	if first && l.hold != nil {
		select {
		case <-l.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	last := ""
	for _, m := range msgs {
		if m.Role == domain.RoleUser {
			last = m.Content
		}
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: domain.FinishActionName, Arguments: map[string]any{"content": "about: " + last}}}}, nil
}

func editRig(t *testing.T, llm domain.LLM) (*runtime.Host, *runtime.HostedAgent, *replyBox, *editMemory) {
	t.Helper()
	box := &replyBox{}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", box)
	mem := &editMemory{memMemory: newMemMemory(), last: map[string]domain.LastExchange{}}
	deps := slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors; d.Memory = mem })
	hosted := &runtime.HostedAgent{Agent: &domain.Agent{ID: "a1", Name: "Kael", LLMs: []domain.LLM{llm}, MaxIterations: 5}, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	return host, hosted, box, mem
}

func typed(id, text string) domain.InboundMessage {
	m := firstMessage(text)
	m.MessageID = id
	return m
}

func edited(id, text string) domain.InboundMessage {
	m := typed(id, text)
	m.Edited = true
	return m
}

func TestEdit_AfterALookUpTheReplyIsRedoneInPlace(t *testing.T) {
	host, hosted, box, mem := editRig(t, &echoLLM{})
	ctx := context.Background()
	host.Receive(ctx, hosted, typed("m1", "rain in London?"))
	waitFor(t, "the reply", func() bool { s, _ := box.snapshot(); return len(s) == 1 })
	host.Receive(ctx, hosted, edited("m1", "rain in Leeds?"))
	waitFor(t, "the new answer in its place", func() bool { _, r := box.snapshot(); return len(r) == 1 })
	sent, replaced := box.snapshot()
	if len(sent) != 1 || replaced[0] != "r1=about: rain in Leeds?" {
		t.Fatalf("in the old reply's place, nothing new sent: %q %q", sent, replaced)
	}
	h := mem.History(ctx, domain.ConversationKey{AgentID: "a1", IdentityID: "id_slack", ChatID: "C1", UserID: "u1"})
	var words []string
	for _, m := range h {
		words = append(words, m.Content)
	}
	if got := strings.Join(words, "|"); !strings.Contains(got, "rain in Leeds?") || strings.Contains(got, "London") {
		t.Errorf("the conversation keeps the new words only: %q", got)
	}
	// Editing it again works the same; an older message's edit changes nothing.
	host.Receive(ctx, hosted, typed("m2", "and Paris?"))
	waitFor(t, "second reply", func() bool { s, _ := box.snapshot(); return len(s) == 2 })
	host.Receive(ctx, hosted, edited("m1", "rain in York?"))
	time.Sleep(100 * time.Millisecond)
	if s, r := box.snapshot(); len(s) != 2 || len(r) != 1 {
		t.Errorf("an older message's edit changes nothing: %q %q", s, r)
	}
}

func TestEdit_AfterAChangeItIsNotRedoneAndTheAgentIsTold(t *testing.T) {
	llm := &echoLLM{}
	host, hosted, box, mem := editRig(t, llm)
	ctx := context.Background()
	host.Receive(ctx, hosted, typed("m1", "archive the Canva newsletters"))
	waitFor(t, "the reply", func() bool { s, _ := box.snapshot(); return len(s) == 1 })
	key := domain.ConversationKey{AgentID: "a1", IdentityID: "id_slack", ChatID: "C1", UserID: "u1"}
	e, _ := mem.LastExchange(ctx, key)
	e.Changed = []string{"Archive Message"}
	mem.SetLastExchange(ctx, key, e)
	host.Receive(ctx, hosted, edited("m1", "archive the Figma newsletters"))
	waitFor(t, "a new message", func() bool { s, _ := box.snapshot(); return len(s) == 2 })
	sent, replaced := box.snapshot()
	if len(replaced) != 0 || sent[1] != "about: archive the Figma newsletters" {
		t.Errorf("the old reply stays, a new message answers: %q %q", sent, replaced)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	last := llm.prompts[len(llm.prompts)-1]
	if !strings.Contains(last, `it said "archive the Canva newsletters" and now says "archive the Figma newsletters"`) || !strings.Contains(last, "already did this: Archive Message") {
		t.Errorf("told what was already done: %q", last)
	}
}

func TestEdit_WhileItIsBeingAnsweredThatReplyIsDropped(t *testing.T) {
	llm := &echoLLM{hold: make(chan struct{})}
	host, hosted, box, _ := editRig(t, llm)
	ctx := context.Background()
	host.Receive(ctx, hosted, typed("m1", "rain in London?"))
	waitFor(t, "being answered", func() bool { llm.mu.Lock(); defer llm.mu.Unlock(); return llm.calls == 1 })
	host.Receive(ctx, hosted, edited("m1", "rain in Leeds?"))
	waitFor(t, "the new words answered", func() bool { s, _ := box.snapshot(); return len(s) == 1 })
	time.Sleep(50 * time.Millisecond)
	if sent, _ := box.snapshot(); len(sent) != 1 || sent[0] != "about: rain in Leeds?" {
		t.Errorf("the old words' reply never sent, the new words answered: %q", sent)
	}
}
