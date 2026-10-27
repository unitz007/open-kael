package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// toolRouter is a router that can judge tools. It records what it was shown and answers from a script.
type toolRouter struct {
	intentRouter
	mu        sync.Mutex
	shown     [][]domain.RoutedSkill
	earlier   []domain.Earlier
	plainUsed int
	answer    map[string][]string
}

func (r *toolRouter) PickSkillsWithTools(_ context.Context, text string, e domain.Earlier, skills []domain.RoutedSkill) ([]string, domain.Intent, error) {
	r.mu.Lock()
	r.shown = append(r.shown, skills)
	r.earlier = append(r.earlier, e)
	r.mu.Unlock()
	in := r.pickIntent
	if in == "" {
		in = domain.IntentRecommend
	}
	return r.answer[text], in, nil
}

func (r *toolRouter) PickSkills(ctx context.Context, text string, s []*domain.Skill) ([]string, domain.Intent, error) {
	r.mu.Lock()
	r.plainUsed++
	r.mu.Unlock()
	return r.intentRouter.PickSkills(ctx, text, s)
}

func (r *toolRouter) PickSkillsInContext(ctx context.Context, text string, e domain.Earlier, s []*domain.Skill) ([]string, domain.Intent, error) {
	r.mu.Lock()
	r.plainUsed++
	r.mu.Unlock()
	return r.intentRouter.PickSkillsInContext(ctx, text, e, s)
}

const statusAsk = "how is my trade doing"

func newToolRouterRig(t *testing.T, on bool) (*followRig, *toolRouter) {
	t.Helper()
	router := &toolRouter{answer: map[string][]string{statusAsk: {"analyse_pair"}}}
	router.byText = map[string][]string{statusAsk: nil} // the plain router would pick nothing
	router.intents = map[string]domain.Intent{statusAsk: domain.IntentRecommend}
	r := newFollowRig(t, router)
	r.host.SetRouteByTools(on)
	return r, router
}

// With routing by tools on, the router is shown each ability with what its tools do, and its answer
// is the one used; the plain picker is not asked.
func TestRouteByTools_TheRouterSeesWhatEachAbilitysToolsDoAndItsAnswerIsUsed(t *testing.T) {
	r, router := newToolRouterRig(t, true)
	withAnotherCapability(r) // with one, there is nothing to pick between
	r.say(t, "C1", statusAsk)

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.shown) != 1 || router.plainUsed != 0 {
		t.Fatalf("asked with tools once, the plain picker not at all: shown=%d plain=%d", len(router.shown), router.plainUsed)
	}
	skills := router.shown[0]
	if len(skills) != 2 || skills[0].Skill.Name != "analyse_pair" {
		t.Fatalf("the agent's ability: %+v", skills)
	}
	if len(skills[0].Tools) != 1 || skills[0].Tools[0].Name != "get_live_quote" {
		t.Errorf("with the tool it can reach: %+v", skills[0].Tools)
	}
	// What the router picked is what runs: the ability's tool was offered to the model.
	if r.spy.calls() != 1 || !strings.Contains(strings.Join(r.spy.offered[0], ","), "get_live_quote") {
		t.Errorf("the ability the router picked ran: %v", r.spy.offered)
	}
}

// Off (the default), or a router that cannot judge tools, behaves exactly as before.
func TestRouteByTools_OffOrWithARouterThatCannotJudgeToolsNothingChanges(t *testing.T) {
	r, router := newToolRouterRig(t, false)
	withAnotherCapability(r)
	r.say(t, "C1", statusAsk)
	router.mu.Lock()
	if len(router.shown) != 0 || router.plainUsed != 1 {
		t.Errorf("off: the plain picker only: shown=%d plain=%d", len(router.shown), router.plainUsed)
	}
	router.mu.Unlock()
	if r.spy.calls() != 1 || strings.Contains(strings.Join(r.spy.offered[0], ","), "get_live_quote") {
		t.Errorf("off: the plain picker found nothing, so no tool: %v", r.spy.offered)
	}

	plain := &intentRouter{intents: map[string]domain.Intent{statusAsk: domain.IntentRecommend}}
	plain.byText = map[string][]string{statusAsk: {"analyse_pair"}}
	r2 := newFollowRig(t, plain)
	r2.host.SetRouteByTools(true) // on, but this router has no such ability
	r2.say(t, "C1", statusAsk)
	if r2.spy.calls() != 1 || !strings.Contains(strings.Join(r2.spy.offered[0], ","), "get_live_quote") {
		t.Errorf("a router that cannot judge tools is used the old way: %v", r2.spy.offered)
	}
}

// Two abilities that grant the same connection are each shown its tools (the router asks about a
// shared tool once), and a follow-up message is shown with what came before it.
func TestRouteByTools_SharedToolsAreShownToEachAbilityAndFollowUpsKeepTheirContext(t *testing.T) {
	integ := &domain.Integration{ID: "int-fx", Service: "fx"}
	quote := &domain.ToolDefinition{ID: "q", Name: "get_live_quote", FunctionName: "get_live_quote", Description: "Returns the current bid and ask price for a symbol.", Action: "q", ReadOnly: true, IntegrationID: integ.ID}
	place := &domain.ToolDefinition{ID: "p", Name: "place_trade", FunctionName: "place_trade", Description: "Place a trade.", Action: "p", IntegrationID: integ.ID}
	analyse := &domain.Skill{ID: "s1", Name: "analyse", Description: "Analyses pairs", AgentID: "a1", Integrations: []string{integ.ID}}
	places := &domain.Skill{ID: "s2", Name: "places", Description: "Places trades", AgentID: "a1", Integrations: []string{integ.ID}, Tools: []domain.ToolBinding{{ToolID: "p"}}}
	spy := &toolSpy{}
	router := &toolRouter{answer: map[string][]string{"yes": {"places"}}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &outbox{})
	executors.Register("fx", &fakeExecutor{})
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{spy}, MaxIterations: 5, Skills: []*domain.Skill{analyse, places}}
	host := runtime.NewHost()
	host.SetSkillRouter(router)
	host.SetRouteByTools(true)
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.ToolsByID = map[string]*domain.ToolDefinition{"q": quote, "p": place}
		d.IntegrationsByID[integ.ID] = integ
	})})
	host.SetUserChannelResolver(func(context.Context, string, string, string) (string, error) { return "u1", nil })
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
	hosted := hostedOf(t, host, "a1")
	if _, err := host.HandleTurn(context.Background(), hosted, conv, "yes"); err != nil {
		t.Fatal(err)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.shown) != 1 || len(router.shown[0]) != 2 {
		t.Fatalf("both abilities: %+v", router.shown)
	}
	names := func(rs domain.RoutedSkill) string {
		var n []string
		for _, tl := range rs.Tools {
			n = append(n, tl.Name)
		}
		return strings.Join(sortedStrings(n), ",")
	}
	if names(router.shown[0][0]) != "get_live_quote,place_trade" || names(router.shown[0][1]) != "get_live_quote,place_trade" {
		t.Errorf("each ability is shown the tools it can reach: %q / %q", names(router.shown[0][0]), names(router.shown[0][1]))
	}
	a, b := router.shown[0][0].Tools, router.shown[0][1].Tools
	for _, ta := range a {
		for _, tb := range b {
			if ta.Name == tb.Name && ta.Key() != tb.Key() {
				t.Errorf("the same tool in two abilities has one key, so it is asked about once: %q vs %q", ta.Key(), tb.Key())
			}
		}
	}
	if (domain.RoutedTool{Name: "a", Description: "b"}).Key() == (domain.RoutedTool{Name: "ab", Description: ""}).Key() {
		t.Error("the key must not mix name and description")
	}
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
