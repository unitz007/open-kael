package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/events"
	"github.com/unitz007/kael/human"
)

// localWiring is satisfied by a real in-process *agent.Agent — RegisterAgent
// checks for it structurally rather than branching on type, so a
// DelegateTarget that isn't a live local agent (a remote peer reached over
// a network boundary, e.g. Claude Code on the owner's laptop) is simply
// registered without this extra wiring, no second registration path
// required anywhere.
type localWiring interface {
	SetEventBus(agent.EventPublisher)
	SetDirectory(agent.AgentDirectory)
	SetWebhookMux(*http.ServeMux)
	SetHuman(human.Human)
}

// starter is satisfied by a real in-process *agent.Agent — Launch only
// starts a registered target's own listener/scheduler loops if it
// implements this; a remote DelegateTarget has nothing of its own to start
// here (it's reached, not run by this process), so it's simply skipped.
type starter interface {
	Start(ctx context.Context) error
}

// Runtime hosts a set of delegate targets — real in-process agents, or
// genuinely remote ones — that share a single event bus, and launches
// whichever of them have something to start.
type Runtime struct {
	targets    []agent.DelegateTarget
	EventBus   *events.EventBus
	webhookMux *http.ServeMux
	human      human.Human
}

// SetHuman wires h onto every currently-registered local agent, and onto
// every one registered after this call — set once here rather than per
// agent, the same way EventBus and the webhook mux are already shared
// through RegisterAgent rather than configured on each agent individually.
func (r *Runtime) SetHuman(h human.Human) {
	r.human = h
	for _, t := range r.targets {
		if lw, ok := t.(localWiring); ok {
			lw.SetHuman(h)
		}
	}
}

func NewRuntime() *Runtime {
	return &Runtime{
		targets:    make([]agent.DelegateTarget, 0),
		EventBus:   events.NewEventBus(),
		webhookMux: http.NewServeMux(),
	}
}

// WebhookHandler returns the shared mux every agent's webhook-triggered
// workflows register onto during Start. Mount this into your own HTTP
// server (alongside /health or whatever else you need) — Runtime still
// doesn't own the port, TLS, or any other route, only the routes its
// agents' workflows actually declare.
func (r *Runtime) WebhookHandler() http.Handler {
	return r.webhookMux
}

// RegisterAgent adds a delegate target to the runtime — a real in-process
// *agent.Agent, or a genuinely remote peer (e.g. Claude Code on the
// owner's laptop, reached over a network boundary) — the one registration
// method for both, no second path. A target that also implements
// localWiring (a real *Agent does) gets wired to the shared event
// bus/directory/webhook mux/human; one that doesn't (a remote peer) is
// simply registered as-is. No-op if a target with the same DelegateID is
// already registered.
func (r *Runtime) RegisterAgent(a agent.DelegateTarget) {
	for _, existing := range r.targets {
		if existing.DelegateID() == a.DelegateID() {
			log.Printf("agent %s already exists in the registry", a.DelegateID())
			return
		}
	}

	if lw, ok := a.(localWiring); ok {
		lw.SetEventBus(r.EventBus)
		lw.SetDirectory(r)
		lw.SetWebhookMux(r.webhookMux)
		if r.human != nil {
			lw.SetHuman(r.human)
		}
	}
	r.targets = append(r.targets, a)

	r.EventBus.PublishEvent(string(events.EventAgentRegistered), a.DelegateName(), "", fmt.Sprintf("Agent %s registered", a.DelegateName()), nil, nil)
}

// Launch starts every registered target that has something to start — a
// real in-process agent mounts its own inbox listener, messenger Listen
// loops, and cron scheduler; a remote DelegateTarget has nothing of its own
// to run in this process, so it's skipped — and blocks until ctx is
// cancelled.
func (r *Runtime) Launch(ctx context.Context) error {
	r.EventBus.PublishEvent(string(events.EventRuntimeStarted), "", "", fmt.Sprintf("Runtime started with %d agent(s)", len(r.targets)), nil, map[string]interface{}{
		"agent_count": len(r.targets),
	})

	for _, t := range r.targets {
		s, ok := t.(starter)
		if !ok {
			continue
		}
		go func(name string, s starter) {
			if err := s.Start(ctx); err != nil {
				log.Printf("Error: failed to start agent %s: %v", name, err)
			}
		}(t.DelegateName(), s)
	}

	log.Printf("🎯kael started successfully with %d agent(s).", len(r.targets))
	<-ctx.Done()
	return nil
}

// DelegateTargets returns every registered target — real or remote — for
// AgentDirectory.
func (r *Runtime) DelegateTargets() []agent.DelegateTarget {
	return r.targets
}

// GetAgents returns only the registered targets that are real in-process
// *agent.Agent values — used by callers (ipc.BuildSnapshot, SendMessage)
// that need concrete Agent fields no DelegateTarget interface exposes. A
// registered remote peer is simply absent from this list, not an error.
func (r *Runtime) GetAgents() []*agent.Agent {
	out := make([]*agent.Agent, 0, len(r.targets))
	for _, t := range r.targets {
		if a, ok := t.(*agent.Agent); ok {
			out = append(out, a)
		}
	}
	return out
}

func (r *Runtime) FindAgent(id string) *agent.Agent {
	for _, a := range r.GetAgents() {
		if a.Id == id {
			return a
		}
	}
	return nil
}
