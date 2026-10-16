package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

type okExec struct{}

func (okExec) Execute(context.Context, *domain.Identity, string, string, map[string]any) (any, error) {
	return "ok", nil
}

type fakeGraph struct {
	mu    sync.RWMutex
	tools map[string]*domain.ToolDefinition
	ids   map[string]*domain.Identity
	ints  map[string]*domain.Integration
}

func (g *fakeGraph) Snapshot() (map[string]*domain.ToolDefinition, map[string]*domain.Identity, map[string]*domain.Integration) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.tools, g.ids, g.ints
}

// A creator's connection made after boot is usable by a skill without a
// restart and without writing to the shared boot maps.
func TestHost_HydratesToolsFromLiveGraph(t *testing.T) {
	execs := domain.NewExecutorRegistry()
	execs.Register("late", okExec{})

	h := NewHost()
	hosted := &HostedAgent{
		Agent: &domain.Agent{ID: "a"},
		Deps: AgentDeps{
			ToolsByID:        map[string]*domain.ToolDefinition{},
			IdentitiesByID:   map[string]*domain.Identity{},
			IntegrationsByID: map[string]*domain.Integration{},
			Executors:        execs,
		},
	}
	skill := &domain.Skill{ID: "s", Integrations: []string{"late_int"}}

	if bound, err := h.hydrate(hosted, skill); err == nil {
		t.Fatalf("unknown integration should fail before the graph is attached, got %d tools", len(bound))
	}

	h.SetLiveGraph(&fakeGraph{
		tools: map[string]*domain.ToolDefinition{"late_int.ping": {ID: "late_int.ping", Name: "ping", IntegrationID: "late_int", Action: "ping"}},
		ids:   map[string]*domain.Identity{},
		ints:  map[string]*domain.Integration{"late_int": {ID: "late_int", Service: "late"}},
	})
	bound, err := h.hydrate(hosted, skill)
	if err != nil || len(bound) != 1 || bound[0].Spec.Name != "ping" {
		t.Fatalf("live tool not hydrated: %v %+v", err, bound)
	}
	if len(hosted.Deps.ToolsByID) != 0 || len(hosted.Deps.IntegrationsByID) != 0 {
		t.Fatal("the boot maps must not be modified")
	}
	if out, err := bound[0].Invoke(context.Background(), nil); err != nil || out != "ok" {
		t.Fatalf("invoke: %v %v", out, err)
	}
}
