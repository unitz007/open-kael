package runtime

import (
	"context"
	"fmt"
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
// A cron fire has no active conversation to request approval in, so a
// RequiresApproval tool bound into a cron-triggered Skill fails outright
// when it fires (see domain.HydrateTool's approval gate) rather than
// running unattended — the same fail-safe stance kael-platform's approval
// gate takes when nothing can ask.
func (h *Host) StartCron(ctx context.Context) (*cron.Cron, error) {
	h.mu.RLock()
	hostedAgents := make([]*HostedAgent, 0, len(h.agents))
	for _, hosted := range h.agents {
		hostedAgents = append(hostedAgents, hosted)
	}
	h.mu.RUnlock()

	c := cron.New()
	for _, hosted := range hostedAgents {
		for _, skill := range hosted.Agent.Skills {
			if skill.Trigger == nil || skill.Trigger.Type != domain.TriggerTypeCron {
				continue
			}
			hosted, skill := hosted, skill // capture per-iteration for the closure below
			_, err := c.AddFunc(skill.Trigger.Value, func() {
				h.runCronSkill(ctx, hosted, skill)
			})
			if err != nil {
				return nil, fmt.Errorf("scheduling agent %q skill %q on %q: %w", hosted.Agent.ID, skill.ID, skill.Trigger.Value, err)
			}
			log.Printf("runtime: agent %q: skill %q scheduled on %q", hosted.Agent.ID, skill.Name, skill.Trigger.Value)
		}
	}
	c.Start()
	return c, nil
}

// runCronSkill fires skill as a headless execution with no input payload.
func (h *Host) runCronSkill(ctx context.Context, hosted *HostedAgent, skill *domain.Skill) {
	h.runSkill(ctx, hosted, skill, nil)
}
