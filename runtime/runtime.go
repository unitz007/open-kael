package runtime

import (
	"context"
	"fmt"
	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/events"
	"github.com/unitz007/kael/human"
	"log"
	"net/http"
	"sync"
)

// Runtime hosts a set of agents that share a single event bus, and launches
// them all together. It's also peer-aware: another Runtime can connect to
// it (see peer.go), and once connected, that peer's own registered agents
// become delegatable from any agent registered here, symmetrically — the
// same Runtime type on both ends of the connection.
type Runtime struct {
	agentRegistry []*agent.Agent
	// peers holds every currently-connected Runtime — see peer.go. Never
	// populated via RegisterAgent; a peer's agents are discovered live, on
	// connect, not statically registered.
	peers      []*Peer
	peersMu    sync.RWMutex
	EventBus   *events.EventBus
	webhookMux *http.ServeMux
	human      human.Human
}

// DelegateTargets satisfies agent.AgentDirectory — every locally-registered
// agent, plus one DelegateTarget per agent any currently-connected peer has
// announced it hosts. Computed fresh on each call: a peer's own registry
// can change over time, and this always reflects what's live right now.
func (r *Runtime) DelegateTargets() []agent.DelegateTarget {
	out := make([]agent.DelegateTarget, 0, len(r.agentRegistry))
	for _, a := range r.agentRegistry {
		out = append(out, a)
	}
	r.peersMu.RLock()
	defer r.peersMu.RUnlock()
	for _, p := range r.peers {
		for _, info := range p.RemoteAgents() {
			out = append(out, &remoteDelegate{info: info, peer: p})
		}
	}
	return out
}

// PeerConnected reports whether agentID is currently reachable through any
// connected peer — the same check DelegateTargets() implicitly does while
// building its list, exposed directly for a caller that wants a yes/no
// answer about one specific ID without needing to scan the whole
// DelegateTargets() result (and without that ID needing to be a real local
// agent at all). Knows nothing about who agentID actually is; that's the
// caller's concern.
func (r *Runtime) PeerConnected(agentID string) bool {
	r.peersMu.RLock()
	defer r.peersMu.RUnlock()
	for _, p := range r.peers {
		for _, info := range p.RemoteAgents() {
			if info.AgentID == agentID {
				return true
			}
		}
	}
	return false
}

// replacePeerLocked evicts any existing peer that announced any of p's own
// agent IDs, then appends p — a fresh connection under the same identity
// supersedes whatever was there before, since the old one is presumably
// already dead or on its way out (confirmed live: without this, a
// reconnect left two entries for the same agent ID, and dispatch kept
// routing to the stale one). Caller must hold peersMu.
func (r *Runtime) replacePeerLocked(p *Peer) {
	newIDs := make(map[string]bool, len(p.remote))
	for _, info := range p.remote {
		newIDs[info.AgentID] = true
	}
	kept := r.peers[:0]
	for _, existing := range r.peers {
		stale := false
		for _, info := range existing.RemoteAgents() {
			if newIDs[info.AgentID] {
				stale = true
				break
			}
		}
		if stale {
			// Failing pending + closing here (rather than waiting for its
			// own readLoop to notice) means anything still waiting on the
			// stale connection fails immediately instead of riding out
			// dispatchTimeout or the keepalive window. Its own readLoop,
			// once its blocked read finally errors, still runs its usual
			// deferred cleanup — that's a harmless no-op here since this
			// filtering has already dropped it from r.peers.
			go existing.evict()
		} else {
			kept = append(kept, existing)
		}
	}
	r.peers = append(kept, p)
}

// SetHuman wires h onto every currently-registered agent, and onto every
// agent registered after this call — set once here rather than per agent,
// the same way EventBus and the webhook mux are already shared through
// RegisterAgent rather than configured on each agent individually.
func (r *Runtime) SetHuman(h human.Human) {
	r.human = h
	for _, a := range r.agentRegistry {
		a.SetHuman(h)
	}
}

func NewRuntime() *Runtime {
	return &Runtime{
		agentRegistry: make([]*agent.Agent, 0),
		EventBus:      events.NewEventBus(),
		webhookMux:    http.NewServeMux(),
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

// RegisterAgent adds an agent to the runtime and wires it to the shared
// event bus and webhook mux. No-op if an agent with the same Id is already
// registered.
func (r *Runtime) RegisterAgent(a *agent.Agent) {
	for _, existing := range r.agentRegistry {
		if existing.Id == a.Id {
			log.Printf("agent %s already exists in the registry", a.Id)
			return
		}
	}

	a.SetEventBus(r.EventBus)
	a.SetDirectory(r)
	a.SetWebhookMux(r.webhookMux)
	if r.human != nil {
		a.SetHuman(r.human)
	}
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
