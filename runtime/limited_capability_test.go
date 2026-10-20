package runtime_test

import (
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A capability limited to some of a connection's abilities offers the model only those, in a real routed turn.
func TestLimitedCapability_TheModelIsOfferedOnlyTheAbilitiesNamed(t *testing.T) {
	offered := func(limit bool) string {
		router := &followRouter{byText: map[string][]string{"what is the price": {"analyse_pair"}}}
		r := newFollowRig(t, router)
		r.hosted.Deps.ToolsByID["place_order"] = &domain.ToolDefinition{ID: "place_order", Name: "place_order", FunctionName: "place_order", Action: "o",
			IntegrationID: "int-fx", Description: "Places an order.", ReadOnly: true, InputSchema: domain.Schema{Type: domain.SchemaTypeObject}}
		if limit {
			r.hosted.Agent.Skills[0].Tools = []domain.ToolBinding{{ToolID: "q"}}
			r.hosted.Agent.Skills[0].LimitedIntegrations = []string{"int-fx"}
		}
		r.host.Register(r.hosted) // so the connection's abilities are read again with the second one in it
		r.say(t, "C1", "what is the price")
		if r.spy.calls() == 0 {
			t.Fatal("the model was not called")
		}
		return strings.Join(r.spy.offered[0], ",")
	}
	if got := offered(false); !strings.Contains(got, "get_live_quote") || !strings.Contains(got, "place_order") {
		t.Errorf("without a limit the whole connection is offered: %s", got)
	}
	if got := offered(true); !strings.Contains(got, "get_live_quote") || strings.Contains(got, "place_order") {
		t.Errorf("limited to the quote, the order ability is never offered: %s", got)
	}
}
