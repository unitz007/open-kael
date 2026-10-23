package runtime

import (
	"context"
	"log"
	"sync/atomic"

	"github.com/unitz007/open-kael/domain"
)

// What the application adds to a person's turn (Host.SetExtraActions: setting up and managing alerts) belongs
// to no ability, so a message asking for it ("let me know whenever there's a new signal", "stop alert 2") matches
// none, and the turn used to end in the fixed "here's what I can do" without the model ever seeing those actions.
// Found in the first live test, 2026-10-23.
//
// Now, when no ability matches and there are such actions, the model is asked once with only them. If it uses one,
// that is the turn (the action's own confirmation and gates apply as always). If it does not, what it wrote is
// thrown away and the turn goes on exactly as before, so nothing else changes.

type extraActionsKey struct{}

func withExtraActions(ctx context.Context, extra []*domain.BoundAction) context.Context {
	if len(extra) == 0 {
		return ctx
	}
	return context.WithValue(ctx, extraActionsKey{}, extra)
}

func extraActionsFrom(ctx context.Context) []*domain.BoundAction {
	extra, _ := ctx.Value(extraActionsKey{}).([]*domain.BoundAction)
	return extra
}

// extraActionsHint tells the model the only thing it may do on this turn.
const extraActionsHint = "None of your abilities fits the person's latest message. The only thing you may do with it here: " +
	"if they ask to be told, or to have something done, when or whenever something happens (an alert), or to see, stop, " +
	"pause or resume their alerts, use the alert actions you have. If the message is about anything else, call finish " +
	"with an empty reply and nothing more."

// tryExtraActions asks the model once with only the extra actions. ok is true when it used one; then its result is the
// turn's.
func (h *Host) tryExtraActions(ctx context.Context, hosted *HostedAgent, messages []domain.Message, actions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, bool) {
	extra := extraActionsFrom(ctx)
	if len(extra) == 0 {
		return nil, nil, false
	}
	var used atomic.Bool
	offered := make([]*domain.BoundAction, 0, len(extra)+1)
	for _, a := range extra {
		invoke := a.Invoke
		offered = append(offered, &domain.BoundAction{Spec: a.Spec, Invoke: func(ctx context.Context, in map[string]any) (any, error) {
			used.Store(true)
			return invoke(ctx, in)
		}})
	}
	for _, a := range actions {
		if a.Spec.Name == domain.FinishActionName {
			offered = append(offered, a)
			break
		}
	}
	result, final, err := h.runNativeLoop(ctx, hosted, withHint(messages, extraActionsHint), offered)
	switch {
	case err != nil:
		log.Printf("skill-router: no ability matched; trying alerts failed: %v — as before", err)
		return nil, nil, false
	case !used.Load():
		return nil, nil, false
	}
	log.Printf("skill-router: no ability matched, but it was about alerts — handled with them")
	h.noteUses(ctx, hosted, nil)
	return result, withoutHint(final, extraActionsHint), true
}
