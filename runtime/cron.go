package runtime

import (
	"context"
	"log"

	"github.com/robfig/cron/v3"
	"github.com/unitz007/open-kael/domain"
)

// StartCron schedules every registered agent's cron-triggered Skills
// (Trigger.Type == domain.TriggerTypeCron, Trigger.Value a standard cron
// expression) and starts the scheduler. Scheduling itself never blocks;
// the caller owns the returned *cron.Cron's lifetime (Stop it on
// shutdown).
//
// The schedule stays in step with the agents afterwards: Register,
// ReloadAgent and Deregister re-sync an agent's entries, so a deleted skill
// stops firing and a new or edited one starts without a restart. A cron
// expression that doesn't parse is logged and skipped — one bad skill must
// not stop the others from running.
//
// A cron fire has no active conversation to request approval in, so a
// RequiresApproval tool bound into a cron-triggered Skill fails outright
// when it fires (see domain.HydrateTool's approval gate) rather than
// running unattended — the same fail-safe stance kael-platform's approval
// gate takes when nothing can ask.
func (h *Host) StartCron(ctx context.Context) (*cron.Cron, error) {
	c := cron.New()
	h.cronMu.Lock()
	h.sched, h.cronCtx = c, ctx
	h.cronEntries = make(map[string][]cron.EntryID)
	h.cronMu.Unlock()

	h.mu.RLock()
	for _, hosted := range h.agents {
		h.syncCron(hosted)
	}
	h.mu.RUnlock()

	c.Start()
	return c, nil
}

// syncCron replaces hosted's cron entries with one per cron-triggered Skill
// it has now. A no-op until StartCron has run. The caller must hold
// whatever protects hosted.Agent.Skills (h.mu).
func (h *Host) syncCron(hosted *HostedAgent) {
	h.cronMu.Lock()
	defer h.cronMu.Unlock()
	if h.sched == nil {
		return
	}
	agentID := hosted.Agent.ID
	for _, id := range h.cronEntries[agentID] {
		h.sched.Remove(id)
	}
	delete(h.cronEntries, agentID)

	for _, skill := range hosted.Agent.Skills {
		if skill.Trigger == nil || skill.Trigger.Type != domain.TriggerTypeCron {
			continue
		}
		skill := skill // capture per-iteration for the closure below
		id, err := h.sched.AddFunc(skill.Trigger.Value, func() {
			h.runCronSkill(h.cronCtx, hosted, skill)
		})
		if err != nil {
			log.Printf("runtime: agent %q: skill %q: invalid cron %q — not scheduled: %v", agentID, skill.Name, skill.Trigger.Value, err)
			continue
		}
		h.cronEntries[agentID] = append(h.cronEntries[agentID], id)
		log.Printf("runtime: agent %q: skill %q scheduled on %q", agentID, skill.Name, skill.Trigger.Value)
	}
}

// dropCron removes every cron entry belonging to agentID.
func (h *Host) dropCron(agentID string) {
	h.cronMu.Lock()
	defer h.cronMu.Unlock()
	if h.sched == nil {
		return
	}
	for _, id := range h.cronEntries[agentID] {
		h.sched.Remove(id)
	}
	delete(h.cronEntries, agentID)
}

// runCronSkill fires skill as a headless execution with no input payload.
func (h *Host) runCronSkill(ctx context.Context, hosted *HostedAgent, skill *domain.Skill) {
	h.runSkill(ctx, hosted, skill, nil)
}
