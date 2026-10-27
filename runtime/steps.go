package runtime

import (
	"context"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

var (
	_ domain.StepWatcher         = (*Host)(nil)
	_ domain.ConversationWatcher = (*Host)(nil)
)

// Steps: what an agent is doing right now (the abilities it uses in what it is handling, and how each went), for its
// creator's screen, and afterwards one record of each message a person sent. Only the abilities' ids and how they went
// are kept: never what went in or came back.

// maxTurnSteps bounds what one piece of work keeps: an agent that loops on an ability is still shown sensibly.
const maxTurnSteps = 30

// turnSteps follows one piece of work. It is the run's domain.StepObserver.
type turnSteps struct {
	h       *Host
	agentID string
	chat    string            // the chat app (identity id) it came in on
	tools   map[string]string // the name the model calls an ability → its id; anything else is not an ability
	plain   map[string]string // the name the model calls an ability → its plain name, for the person
	shower  domain.StepShower // the chat app, when it shows the person what is happening
	conv    domain.ConversationRef

	mu    sync.Mutex
	caps  []string
	steps []domain.LiveStep
}

// beginSteps starts following a piece of work, as the agent's latest, in the conversation it came in on; its chat app
// is told each step too when it shows them (domain.StepShower).
func (h *Host) beginSteps(hosted *HostedAgent, conv domain.ConversationRef) *turnSteps {
	tools, _, _ := hosted.View()
	names := make(map[string]string, len(tools))
	plain := make(map[string]string, len(tools))
	for id, def := range tools {
		names[def.ModelName()] = id
		plain[def.ModelName()] = def.Name
		if def.Name == "" {
			plain[def.ModelName()] = def.ModelName()
		}
	}
	t := &turnSteps{h: h, agentID: hosted.Agent.ID, chat: conv.IdentityID, tools: names, plain: plain, conv: conv}
	if _, integration := resolveIdentityAndIntegration(hosted, conv); integration != nil && hosted.Deps.Executors != nil {
		if ex, ok := hosted.Deps.Executors.For(integration.Service); ok {
			t.shower, _ = ex.(domain.StepShower)
		}
	}
	b := &h.work
	b.mu.Lock()
	if b.live == nil {
		b.live = map[string][]*turnSteps{}
	}
	b.live[t.agentID] = append(b.live[t.agentID], t)
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
	if t.shower != nil {
		t.shower.ShowStep(t.conv, t.plain[action], state)
	}
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
	return domain.LiveTurn{Capabilities: append([]string{}, t.caps...), Steps: append([]domain.LiveStep{}, t.steps...), Chat: t.chat}
}

// end stops following it, and tells its chat app the work is over.
func (t *turnSteps) end() {
	if t.shower != nil {
		t.shower.WorkDone(t.conv)
	}
	b := &t.h.work
	b.mu.Lock()
	list := b.live[t.agentID]
	for i, x := range list {
		if x == t {
			list = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(b.live, t.agentID)
	} else {
		b.live[t.agentID] = list
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

// LiveTurns implements domain.StepWatcher: each agent's latest piece of work.
func (h *Host) LiveTurns(agentIDs []string) map[string]domain.LiveTurn {
	out := map[string]domain.LiveTurn{}
	for id, list := range h.LiveConversations(agentIDs) {
		out[id] = list[len(list)-1]
	}
	return out
}

// LiveConversations implements domain.ConversationWatcher: every piece of work in progress, oldest first.
func (h *Host) LiveConversations(agentIDs []string) map[string][]domain.LiveTurn {
	b := &h.work
	b.mu.Lock()
	live := make(map[string][]*turnSteps, len(agentIDs))
	for _, id := range agentIDs {
		if list := b.live[id]; len(list) > 0 {
			live[id] = append([]*turnSteps(nil), list...)
		}
	}
	b.mu.Unlock()
	out := make(map[string][]domain.LiveTurn, len(live))
	for id, list := range live {
		for _, t := range list {
			out[id] = append(out[id], t.snapshot())
		}
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
