package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// ctxExecutor records who the work was being done for, as seen by a tool.
type ctxExecutor struct {
	mu   sync.Mutex
	seen []domain.UserDetails
}

func (c *ctxExecutor) Execute(ctx context.Context, _ *domain.Identity, _ string, _ string, _ map[string]any) (any, error) {
	c.mu.Lock()
	c.seen = append(c.seen, domain.UserDetailsFromContext(ctx))
	c.mu.Unlock()
	return "ok", nil
}

type systemCapture struct {
	system chan string
}

func (s *systemCapture) Run(_ context.Context, messages []domain.Message, _ []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	for _, m := range messages {
		if m.Role == domain.RoleSystem {
			s.system <- m.Content
		}
	}
	return &domain.LoopResult{Status: domain.LoopStatusComplete}, messages, nil
}

var ada = domain.UserDetails{Name: "Ada", Timezone: "Africa/Lagos", Language: "fr", Country: "NG"}

// A repeating task has no chat to carry the person, so the turn loads them: the morning a daily
// summary reports on is theirs, not the server's.
func TestScheduledRun_TheTaskKnowsWhoItIsFor(t *testing.T) {
	cap := &systemCapture{system: make(chan string, 2)}
	agent := &domain.Agent{ID: "a1", Name: "Desk", Loop: cap}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &outbox{})
	host := runtime.NewHost()
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) { d.Executors = executors })})
	host.SetUserDetailsLoader(func(context.Context, string) (domain.UserDetails, error) { return ada, nil })

	if err := host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "summarise my day"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-cap.system:
		if !strings.Contains(got, `called "Ada"`) || !strings.Contains(got, "Africa/Lagos") || !strings.Contains(got, "preferred language is French") {
			t.Errorf("a repeating task must know the person and their time zone: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the task never ran")
	}
}

// In a chat turn the person travels with the turn into the tools an ability runs.
func TestChatTurn_TheAbilitiesThatRunKnowWhoTheyAreFor(t *testing.T) {
	exec := &ctxExecutor{}
	integ := &domain.Integration{ID: "int-tool", Service: "tool"}
	tool := &domain.ToolDefinition{ID: "t1", Name: "do_thing", IntegrationID: integ.ID, Action: "do_thing"}
	skill := &domain.Skill{ID: "s1", Name: "do_thing_skill", AgentID: "a1", Tools: []domain.ToolBinding{{ToolID: "t1"}}}
	llm := &funcLLM{call: func(n int) (*domain.LLMResponse, error) {
		if n == 1 {
			return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "do_thing_skill", Arguments: map[string]any{}}}}, nil
		}
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "2", Name: domain.FinishActionName, Arguments: map[string]any{"content": "done"}}}}, nil
	}}
	agent := &domain.Agent{ID: "a1", Name: "Desk", Skills: []*domain.Skill{skill}, LLMs: []domain.LLM{llm}, MaxIterations: 5}
	executors := domain.NewExecutorRegistry()
	executors.Register("tool", exec)
	executors.Register("slack", &outbox{})
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = newMemMemory()
		d.ToolsByID = map[string]*domain.ToolDefinition{"t1": tool}
		d.IntegrationsByID[integ.ID] = integ
	})
	hosted := &runtime.HostedAgent{Agent: agent, Deps: deps}
	host := runtime.NewHost()
	host.Register(hosted)
	host.SetUserDetailsLoader(func(context.Context, string) (domain.UserDetails, error) { return ada, nil })

	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1"}
	if _, err := host.HandleTurn(context.Background(), hosted, conv, "do the thing"); err != nil {
		t.Fatal(err)
	}
	if len(exec.seen) != 1 || exec.seen[0] != ada {
		t.Fatalf("the ability must be run knowing who it is for: %+v", exec.seen)
	}
}
