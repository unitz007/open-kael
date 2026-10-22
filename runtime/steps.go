package runtime

import (
	"context"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.StepWatcher = (*Host)(nil)

// Steps: what an agent is doing right now (the abilities it uses in what it is handling, and how each went), for its
// creator's screen, and afterwards one record of each message a person sent. Only the abilities' ids and how they went
// are kept: never what went in or came back.

// maxTurnSteps bounds what one piece of work keeps: an agent that loops on an ability is still shown sensibly.
const maxTurnSteps = 30

// turnSteps follows one piece of work. It is the run's domain.StepObserver.
type turnSteps struct {
	h       *Host
	agentID string
	tools   map[string]string // the name the model calls an ability → its id; anything else is not an ability

	mu    sync.Mutex
	caps  []string
	steps []domain.LiveStep
}

// beginSteps starts following a piece of work, as the agent's latest.
func (h *Host) beginSteps(hosted *HostedAgent) *turnSteps {
	tools, _, _ := hosted.View()
	names := make(map[string]string, len(tools))
	for id, def := range tools {
		names[def.ModelName()] = id
	}
	t := &turnSteps{h: h, agentID: hosted.Agent.ID, tools: names}
	b := &h.work
	b.mu.Lock()
	if b.live == nil {
		b.live = map[string]*turnSteps{}
	}
	b.live[t.agentID] = t
	b.mu.Unlock()
	return t
}

// Step implements domain.StepObserver: a use that has not ended yet is updated, otherwise a new use is added.
func (t *turnSteps) Step(action string, state domain.StepState) {
	id, ok := t.tools[action]
	if !ok {
		return // not one of the agent's abilities (finishing, asking, the platform's own actions)
	}
	t.mu.Lock()
	updated := false
	for i := len(t.steps) - 1; i >= 0; i-- {
		if t.steps[i].Tool == id && !t.steps[i].State.Ended() {
			t.steps[i].State = state
			updated = true
			break
		}
	}
	if !updated {
		if len(t.steps) >= maxTurnSteps {
			t.mu.Unlock()
			return
		}
		t.steps = append(t.steps, domain.LiveStep{Tool: id, State: state})
	}
	t.mu.Unlock()
	t.h.work.notify(t.agentID)
}

// setCapabilities notes which capabilities (ids) routing chose for this piece of work.
func (t *turnSteps) setCapabilities(ids []string) {
	t.mu.Lock()
	t.caps = append([]string(nil), ids...)
	t.mu.Unlock()
	t.h.work.notify(t.agentID)
}

func (t *turnSteps) snapshot() domain.LiveTurn {
	t.mu.Lock()
	defer t.mu.Unlock()
	return domain.LiveTurn{Capabilities: append([]string{}, t.caps...), Steps: append([]domain.LiveStep{}, t.steps...)}
}

// end stops following it (if it is still the agent's latest).
func (t *turnSteps) end() {
	b := &t.h.work
	b.mu.Lock()
	if b.live[t.agentID] == t {
		delete(b.live, t.agentID)
	}
	b.mu.Unlock()
}

// stepsOf returns the piece of work being followed in ctx, if any.
func stepsOf(ctx context.Context) (*turnSteps, bool) {
	o, ok := domain.StepObserverFromContext(ctx)
	if !ok {
		return nil, false
	}
	t, ok := o.(*turnSteps)
	return t, ok
}

// LiveTurns implements domain.StepWatcher.
func (h *Host) LiveTurns(agentIDs []string) map[string]domain.LiveTurn {
	b := &h.work
	b.mu.Lock()
	live := make(map[string]*turnSteps, len(agentIDs))
	for _, id := range agentIDs {
		if t := b.live[id]; t != nil {
			live[id] = t
		}
	}
	b.mu.Unlock()
	out := make(map[string]domain.LiveTurn, len(live))
	for id, t := range live {
		out[id] = t.snapshot()
	}
	return out
}

// noteTurn keeps one record of a message a person sent: its capabilities, its steps, and whether the reply went
// through. Alerts and repeating tasks are not someone using the agent, so they are not kept.
func (h *Host) noteTurn(ctx context.Context, t *turnSteps, err error) {
	tr, ok := h.activity.(domain.TurnRecorder)
	if !ok || domain.IsUnattended(ctx) {
		return
	}
	live := t.snapshot()
	rec := domain.TurnRecord{Capabilities: live.Capabilities, Steps: live.Steps, Failed: err != nil}
	agentID := t.agentID
	h.record(func(ctx context.Context) { tr.TurnHandled(ctx, agentID, rec) })
}
