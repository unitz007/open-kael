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

// ChildRuntime is the minimal surface a runtime needs to host agents at
// all — register them, report them for delegation, run them. This is the
// whole interface a runtime with no webhooks/human/shared-event-bus backing
// (the owner's own laptop, hosting one or more CLI-shaped agents) can
// genuinely satisfy; it never has to carry capabilities it has no real
// implementation for.
type ChildRuntime interface {
	agent.AgentDirectory // DelegateTargets() []agent.DelegateTarget
	RegisterAgent(a agent.DelegateTarget)
	Launch(ctx context.Context) error
}

// ParentRuntime is the full set a fully-hosted runtime (Fly) provides —
// everything ChildRuntime offers, plus webhook routing, human details, a
// shared event bus for the TUI/dashboard, and lookup of real in-process
// agents by concrete type. *LocalRuntime satisfies this in full; a
// distributed variant that folds in remote peers (see the remoteagent
// package) wraps a *LocalRuntime and satisfies it too.
type ParentRuntime interface {
	ChildRuntime
	GetAgents() []*agent.Agent
	FindAgent(id string) *agent.Agent
	WebhookHandler() http.Handler
	SetHuman(h human.Human)
	EventBus() *events.EventBus
}

// LocalRuntime hosts a set of delegate targets — real in-process agents, or
// genuinely remote ones — that share a single event bus, and launches
// whichever of them have something to start. Implements ParentRuntime in
// full, and — being structural — ChildRuntime for free.
type LocalRuntime struct {
	targets    []agent.DelegateTarget
	eventBus   *events.EventBus
	webhookMux *http.ServeMux
	human      human.Human
}

// SetHuman wires h onto every currently-registered local agent, and onto
// every one registered after this call — set once here rather than per
// agent, the same way EventBus and the webhook mux are already shared
// through RegisterAgent rather than configured on each agent individually.
func (r *LocalRuntime) SetHuman(h human.Human) {
	r.human = h
	for _, t := range r.targets {
		if lw, ok := t.(localWiring); ok {
			lw.SetHuman(h)
		}
	}
}

// NewRuntime builds a *LocalRuntime — the name stays NewRuntime (not
// NewLocalRuntime) since every existing caller already spells it this way;
// only the return type changed as part of the ParentRuntime/ChildRuntime
// split.
func NewRuntime() *LocalRuntime {
	return &LocalRuntime{
		targets:    make([]agent.DelegateTarget, 0),
		eventBus:   events.NewEventBus(),
		webhookMux: http.NewServeMux(),
	}
}

// EventBus returns the shared event bus every registered local agent
// publishes lifecycle/workflow events onto — a method, not a field, since
// ParentRuntime is an interface and interfaces can't expose fields.
func (r *LocalRuntime) EventBus() *events.EventBus {
	return r.eventBus
}

// WebhookHandler returns the shared mux every agent's webhook-triggered
// workflows register onto during Start. Mount this into your own HTTP
// server (alongside /health or whatever else you need) — LocalRuntime still
// doesn't own the port, TLS, or any other route, only the routes its
// agents' workflows actually declare.
func (r *LocalRuntime) WebhookHandler() http.Handler {
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
func (r *LocalRuntime) RegisterAgent(a agent.DelegateTarget) {
	for _, existing := range r.targets {
		if existing.DelegateID() == a.DelegateID() {
			log.Printf("agent %s already exists in the registry", a.DelegateID())
			return
		}
	}

	if lw, ok := a.(localWiring); ok {
		lw.SetEventBus(r.eventBus)
		lw.SetDirectory(r)
		lw.SetWebhookMux(r.webhookMux)
		if r.human != nil {
			lw.SetHuman(r.human)
		}
	}
	r.targets = append(r.targets, a)

	r.eventBus.PublishEvent(string(events.EventAgentRegistered), a.DelegateName(), "", fmt.Sprintf("Agent %s registered", a.DelegateName()), nil, nil)
}

// Launch starts every registered target that has something to start — a
// real in-process agent mounts its own inbox listener, messenger Listen
// loops, and cron scheduler; a remote DelegateTarget has nothing of its own
// to run in this process, so it's skipped — and blocks until ctx is
// cancelled.
func (r *LocalRuntime) Launch(ctx context.Context) error {
	r.eventBus.PublishEvent(string(events.EventRuntimeStarted), "", "", fmt.Sprintf("Runtime started with %d agent(s)", len(r.targets)), nil, map[string]interface{}{
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
func (r *LocalRuntime) DelegateTargets() []agent.DelegateTarget {
	return r.targets
}

// GetAgents returns only the registered targets that are real in-process
// *agent.Agent values — used by callers (ipc.BuildSnapshot, SendMessage)
// that need concrete Agent fields no DelegateTarget interface exposes. A
// registered remote peer is simply absent from this list, not an error.
func (r *LocalRuntime) GetAgents() []*agent.Agent {
	out := make([]*agent.Agent, 0, len(r.targets))
	for _, t := range r.targets {
		if a, ok := t.(*agent.Agent); ok {
			out = append(out, a)
		}
	}
	return out
}

func (r *LocalRuntime) FindAgent(id string) *agent.Agent {
	for _, a := range r.GetAgents() {
		if a.Id == id {
			return a
		}
	}
	return nil
}
