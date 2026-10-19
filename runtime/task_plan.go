package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/unitz007/open-kael/domain"
)

// TaskPlan says what a task would need in order to change things when it runs with nobody there.
type TaskPlan struct {
	// Allowed are the actions the task could be given leave to change things with: the ones that fit
	// what it asks for and need nobody's approval.
	Allowed []string
	// NeedsApproval are actions that fit it too but are set (by the creator, or by the person for
	// themselves) to ask for approval every time. They can never run with nobody there.
	NeedsApproval []string
}

// ErrCannotPlan means it could not be worked out which actions a task would need, so none can be
// granted: leave to change things is never given on a guess.
var ErrCannotPlan = errors.New("could not work out which actions this task needs")

// PlanTaskChanges works out, before a repeating task is saved, which actions it would need to
// change things: the same question the router asks of a request to DO something (which actions
// would make it done), asked of the stored words. The person is then shown it and agrees to exactly
// that, no more. Actions that ask for approval are listed apart, never granted.
func (h *Host) PlanTaskChanges(ctx context.Context, agentID, userID, task string) (TaskPlan, error) {
	h.mu.RLock()
	hosted, ok := h.agents[agentID]
	h.mu.RUnlock()
	if !ok {
		return TaskPlan{}, fmt.Errorf("PlanTaskChanges: agent %q not registered", agentID)
	}
	if h.skillRouter == nil || h.toolRequirer == nil {
		return TaskPlan{}, ErrCannotPlan
	}

	skillNames, _, err := h.skillRouter.PickSkills(ctx, task, hosted.Agent.Skills)
	if err != nil {
		return TaskPlan{}, fmt.Errorf("%w: %v", ErrCannotPlan, err)
	}
	seen := map[string]bool{}
	var changing []*domain.BoundAction
	for _, name := range skillNames {
		for _, skill := range hosted.Agent.Skills {
			if skill.Name != name {
				continue
			}
			tools, err := h.hydrate(hosted, skill)
			if err != nil {
				continue
			}
			for _, t := range tools {
				if t.Spec.Changes && !seen[t.Spec.Name] {
					seen[t.Spec.Name] = true
					changing = append(changing, t)
				}
			}
		}
	}
	if len(changing) == 0 {
		return TaskPlan{}, nil
	}
	names, err := h.toolRequirer.RequiredTools(ctx, task, "", changing)
	if err != nil {
		return TaskPlan{}, fmt.Errorf("%w: %v", ErrCannotPlan, err)
	}

	// Which of them ask for approval, by the creator or by this person's own settings.
	asks := map[string]bool{}
	var personFlags map[string]bool
	if userID != "" && h.userToolApprovalLoader != nil {
		if m, err := h.userToolApprovalLoader(ctx, userID); err == nil {
			personFlags = m
		} else {
			log.Printf("runtime: plan task: load tool approvals for %q: %v", userID, err)
			return TaskPlan{}, fmt.Errorf("%w: %v", ErrCannotPlan, err) // can't see their settings: grant nothing
		}
	}
	tools, _, _ := hosted.View()
	for _, def := range tools {
		if def.RequiresApproval || personFlags[def.ID] {
			asks[def.ModelName()] = true
		}
	}

	var plan TaskPlan
	for _, n := range names {
		if asks[n] {
			plan.NeedsApproval = append(plan.NeedsApproval, n)
		} else {
			plan.Allowed = append(plan.Allowed, n)
		}
	}
	sort.Strings(plan.Allowed)
	sort.Strings(plan.NeedsApproval)
	return plan, nil
}
