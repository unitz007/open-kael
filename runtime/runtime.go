package runtime

import (
	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/events"
	"context"
	"fmt"
	"log"
)

// Runtime hosts a set of agents that share a single event bus, and launches
// them all together.
type Runtime struct {
	agentRegistry []*agent.Agent
	EventBus      *events.EventBus
}

func NewRuntime() *Runtime {
	return &Runtime{
		agentRegistry: make([]*agent.Agent, 0),
		EventBus:      events.NewEventBus(),
	}
}

// RegisterAgent adds an agent to the runtime and wires it to the shared
// event bus. No-op if an agent with the same Id is already registered.
func (r *Runtime) RegisterAgent(a *agent.Agent) {
	for _, existing := range r.agentRegistry {
		if existing.Id == a.Id {
			log.Printf("agent %s already exists in the registry", a.Id)
			return
		}
	}

	a.SetEventBus(r.EventBus)
	a.SetDirectory(r)
	r.agentRegistry = append(r.agentRegistry, a)

	r.EventBus.PublishEvent(string(events.EventAgentRegistered), a.Name, "", fmt.Sprintf("Agent %s registered", a.Name), nil, nil)
}

// Launch starts every registered agent — each mounts its own inbox
// listener, messenger Listen loops, and cron scheduler — and blocks until
// ctx is cancelled.
func (r *Runtime) Launch(ctx context.Context) error {
	r.EventBus.PublishEvent(string(events.EventRuntimeStarted), "", "", fmt.Sprintf("Runtime started with %d agent(s)", len(r.agentRegistry)), nil, map[string]interface{}{
		"agent_count": len(r.agentRegistry),
	})

	for _, a := range r.agentRegistry {
		go func(a *agent.Agent) {
			if err := a.Start(ctx); err != nil {
				log.Printf("Error: failed to start agent %s: %v", a.Name, err)
			}
		}(a)
	}

	log.Printf("🎯kael started successfully with %d agent(s).", len(r.agentRegistry))
	<-ctx.Done()
	return nil
}

func (r *Runtime) GetAgents() []*agent.Agent {
	return r.agentRegistry
}

func (r *Runtime) FindAgent(id string) *agent.Agent {
	for _, a := range r.agentRegistry {
		if a.Id == id {
			return a
		}
	}
	return nil
}
