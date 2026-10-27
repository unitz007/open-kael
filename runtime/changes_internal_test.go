package runtime

import (
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A request to do something keeps the changes of the apps it is about (trimming or its requirements picked something
// from them) and the platform's own actions; another app's changes aren't offered.
func TestWithChanges_OnlyTheAppsTheRequestIsAbout(t *testing.T) {
	a := func(name, app string, readOnly bool) *domain.BoundAction {
		return &domain.BoundAction{Spec: domain.ActionSpec{Name: name, ReadOnly: readOnly}, App: app}
	}
	all := []*domain.BoundAction{
		a("get_live_quote", "trading", true), a("place_trade", "trading", false),
		a("list_events", "calendar", true), a("create_event", "calendar", false),
		a("make_transfer", "fpl", false), a("complete_task", "tasks", false),
		a("repeating_tasks", "", false),
	}
	filtered := []*domain.BoundAction{all[0]}
	got := withChanges(filtered, all, map[string]bool{"get_live_quote": true, "complete_task": true})
	if names := strings.Join(actionNames(got), ","); names != "get_live_quote,place_trade,complete_task,repeating_tasks" {
		t.Errorf("offered %s", names)
	}
}
