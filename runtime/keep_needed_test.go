package runtime

import (
	"sort"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func names(as []*domain.BoundAction) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Spec.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// What trimming left out comes back when an offered ability relies on it: what it must run first, the look-ups it
// declares its inputs come from, and, for a connection that declares nothing (a creator's own), all its look-ups.
func TestKeepNeeded(t *testing.T) {
	act := func(name, app string, readOnly, declared bool, needs, with []string) *domain.BoundAction {
		return &domain.BoundAction{Spec: domain.ActionSpec{Name: name, ReadOnly: readOnly}, App: app, Declared: declared, Needs: needs, With: with}
	}
	transfer := act("make_transfer", "fpl", false, true, []string{"get_my_team"}, []string{"list_players"})
	team := act("get_my_team", "fpl", true, true, nil, nil)
	players := act("list_players", "fpl", true, true, nil, nil)
	fixtures := act("get_fixtures", "fpl", true, true, nil, nil)
	quote := act("get_quote", "finos", true, false, nil, nil)
	signals := act("get_latest_decisions", "finos", true, false, nil, nil)
	email := act("send_email", "gmail", false, true, nil, nil)
	all := []*domain.BoundAction{transfer, team, players, fixtures, quote, signals, email}

	if got := names(keepNeeded([]*domain.BoundAction{transfer}, all)); got != "get_my_team,list_players,make_transfer" {
		t.Errorf("a declaring app brings exactly what it lists: %s", got)
	}
	if got := names(keepNeeded([]*domain.BoundAction{quote}, all)); got != "get_latest_decisions,get_quote" {
		t.Errorf("a connection that lists nothing brings all its look-ups: %s", got)
	}
	if got := names(keepNeeded([]*domain.BoundAction{email}, all)); got != "send_email" {
		t.Errorf("nothing listed, nothing added: %s", got)
	}
}
