package runtime

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

// triggerInputKey is the input field a Skill's InputSchema should declare to
// receive the event payload — the string EventSource.Ingest returns.
const triggerInputKey = "trigger_input"

// notifyConversationInputKey is the input field carrying the destination
// ConversationRef when Trigger.NotifyConversation is set — lets a tool post
// progress updates to that conversation while it's still running.
const notifyConversationInputKey = "notify_conversation"

// EventBus dispatches named events to every Skill subscribed to them.
// Subscriptions are registered once at startup via RegisterEventSources or
// by direct Subscribe calls; Emit can be called from anywhere in the system
// (HTTP ingestion, internal lifecycle events, etc.).
type EventBus struct {
	mu   sync.RWMutex
	subs map[string][]*eventSub
}

type eventSub struct {
	hosted *HostedAgent
	skill  *domain.Skill
}

func newEventBus() *EventBus {
	return &EventBus{subs: make(map[string][]*eventSub)}
}

// Subscribe registers hosted's skill to fire whenever eventName is emitted.
// Safe to call before or after the bus starts receiving events.
func (b *EventBus) Subscribe(eventName string, hosted *HostedAgent, skill *domain.Skill) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventName] = append(b.subs[eventName], &eventSub{hosted: hosted, skill: skill})
}

// emit delivers eventName to all subscribed Skills, running each in its own
// goroutine so a slow skill never blocks others.
//
// sourceIdentityID is the Identity whose webhook secret verified this
// delivery; actorExternalID is the provider's user ID for the sender (e.g.
// a GitHub login). When both are non-empty and h.eventActorRefLoader is set,
// the actor's AppAuthorizations are resolved and injected into the skill's
// context via WithConnectionRefs — enabling the skill to act on behalf of
// the sender using their own per-provider credentials.
func (b *EventBus) emit(ctx context.Context, h *Host, eventName, payload, sourceIdentityID, actorExternalID string) {
	b.mu.RLock()
	subs := b.subs[eventName]
	b.mu.RUnlock()

	// Resolve connection refs for the event's actor once, shared across all
	// subscribed skills for this delivery.
	if actorExternalID != "" && sourceIdentityID != "" && h.eventActorRefLoader != nil {
		refs, err := h.eventActorRefLoader(ctx, sourceIdentityID, actorExternalID)
		if err != nil {
			log.Printf("runtime: event %q: resolve actor %q: %v (proceeding without user refs)", eventName, actorExternalID, err)
		} else if len(refs) > 0 {
			ctx = domain.WithConnectionRefs(ctx, refs)
		}
	}

	for _, sub := range subs {
		sub := sub
		go h.runEventSkill(ctx, sub.hosted, sub.skill, payload)
	}
}

// RegisterEventSources does two things:
//  1. Subscribes every registered agent's event-triggered Skills to the bus
//     so they fire when their event name is emitted.
//  2. Wires an HTTP handler for each EventSource onto mux so external
//     services can POST events — the handler verifies the signature, calls
//     Ingest to get the canonical event name, payload, and actor, then emits
//     to the bus. Skills whose Trigger.Value doesn't match any event that any
//     source can produce will simply never fire from HTTP; they may still fire
//     from internal Emit calls.
func (h *Host) RegisterEventSources(mux *http.ServeMux, sources *domain.EventSourceRegistry) {
	h.mu.RLock()
	hostedAgents := make([]*HostedAgent, 0, len(h.agents))
	for _, hosted := range h.agents {
		hostedAgents = append(hostedAgents, hosted)
	}
	h.mu.RUnlock()

	// Build bus subscriptions from every event-triggered skill.
	for _, hosted := range hostedAgents {
		for _, skill := range hosted.Agent.Skills {
			if skill.Trigger == nil || skill.Trigger.Type != domain.TriggerTypeEvent {
				continue
			}
			h.bus.Subscribe(skill.Trigger.Value, hosted, skill)
			log.Printf("runtime: agent %q: skill %q subscribed to event %q", hosted.Agent.ID, skill.Name, skill.Trigger.Value)
		}
	}

	// Register one HTTP handler per source path.
	for _, entry := range sources.All() {
		entry := entry
		mux.HandleFunc(entry.Source.Path(), func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			if !entry.Source.Verify(body, r.Header) {
				http.Error(w, "signature verification failed", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			for _, ev := range entry.Events {
				payload, actorExternalID, ok, err := ev.Handler(body, r.Header)
				if err != nil {
					log.Printf("runtime: event handler %q: %v", ev.Name, err)
					return
				}
				if ok {
					h.bus.emit(context.Background(), h, ev.Name, payload, entry.IdentityID, actorExternalID)
					return
				}
			}
		})
		log.Printf("runtime: event source registered at %s", entry.Source.Path())
	}
}

// Emit fires eventName on the bus — the internal path for system-generated
// events ("user.channel.connected", "agent.turn.completed", etc.).
func (h *Host) Emit(ctx context.Context, eventName, payload string) {
	h.bus.emit(ctx, h, eventName, payload, "", "")
}

// runEventSkill fires skill with the event payload as trigger_input.
func (h *Host) runEventSkill(ctx context.Context, hosted *HostedAgent, skill *domain.Skill, payload string) {
	h.runSkill(ctx, hosted, skill, map[string]any{triggerInputKey: payload})
}

func stringifyOutput(output any) string {
	switch v := output.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["result"].(string); ok {
			return s
		}
	}
	return fmt.Sprintf("%v", output)
}
