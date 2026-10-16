package domain_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func tool(integration, name string) *domain.ToolDefinition {
	return &domain.ToolDefinition{
		ID: integration + "." + name, Name: name, IntegrationID: integration, Action: name, Description: "Does " + name + ".",
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject},
	}
}

func source(integration, owner string, names ...string) domain.StaticSource {
	s := domain.StaticSource{Integ: &domain.Integration{ID: integration, Service: integration, CreatedBy: owner},
		Idents: []*domain.Identity{{ID: "id_" + integration, IntegrationID: integration}}}
	for _, n := range names {
		s.ToolDefs = append(s.ToolDefs, tool(integration, n))
	}
	return s
}

func TestCatalog_AddSourceReplacesWhatItHeldForThatIntegration(t *testing.T) {
	c := domain.NewCatalog()
	if err := c.AddSource(source("a", "", "one", "two")); err != nil {
		t.Fatal(err)
	}
	if err := c.AddSource(source("b", "", "other")); err != nil {
		t.Fatal(err)
	}
	if len(c.ToolsOf("a")) != 2 {
		t.Fatalf("tools of a: %d", len(c.ToolsOf("a")))
	}

	// Registering a again with a different set swaps it whole: "two" goes, "three" arrives.
	if err := c.AddSource(source("a", "", "one", "three")); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Tool("a.two"); ok {
		t.Error("a tool no longer offered must be gone")
	}
	if _, ok := c.Tool("a.three"); !ok {
		t.Error("the new tool must be there")
	}
	if len(c.ToolsOf("b")) != 1 {
		t.Error("another integration is untouched")
	}
	if _, ok := c.Identity("id_a"); !ok {
		t.Error("identities arrive with the source")
	}

	c.RemoveSource("a")
	if _, ok := c.Integration("a"); ok || len(c.ToolsOf("a")) != 0 {
		t.Error("removing a source removes the integration and its tools")
	}
	if _, ok := c.Identity("id_a"); ok {
		t.Error("and its identities")
	}
	if _, ok := c.Integration("b"); !ok {
		t.Error("others stay")
	}
}

func TestCatalog_RefusesToolsThatBreakTheContractButKeepsTheRest(t *testing.T) {
	c := domain.NewCatalog()
	bad := tool("a", "broken")
	bad.Description = ""
	badName := tool("a", "x")
	badName.Name = "has space"
	foreign := tool("other", "stray") // claims to belong elsewhere
	src := source("a", "", "good")
	src.ToolDefs = append(src.ToolDefs, bad, badName, foreign)

	err := c.AddSource(src)
	if err == nil {
		t.Fatal("tools that break the contract must be reported")
	}
	for _, want := range []string{"no description", "name the model sees", `belongs to integration "other"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if _, ok := c.Tool("a.good"); !ok {
		t.Error("one bad tool must not take the whole integration down")
	}
	for _, id := range []string{"a.broken", "a.x", "other.stray"} {
		if _, ok := c.Tool(id); ok {
			t.Errorf("%s was refused and must not be in the catalog", id)
		}
	}
}

func TestCatalog_NamesStayDistinctWithinAnIntegration(t *testing.T) {
	c := domain.NewCatalog()
	src := source("a", "", "same")
	dup := tool("a", "same")
	dup.ID = "a.other_id"
	src.ToolDefs = append(src.ToolDefs, dup)
	err := c.AddSource(src)
	if err == nil || !strings.Contains(err.Error(), "same name") {
		t.Fatalf("two tools the model can't tell apart: %v", err)
	}
	if len(c.ToolsOf("a")) != 1 {
		t.Fatalf("the first is kept, the duplicate refused: %d", len(c.ToolsOf("a")))
	}
	// The same name in a different integration is fine.
	if err := c.AddSource(source("b", "", "same")); err != nil {
		t.Fatal(err)
	}
}

func TestCatalog_SnapshotsAreIsolated(t *testing.T) {
	c := domain.NewCatalog()
	_ = c.AddSource(source("a", "", "one"))
	snap := c.Tools()
	delete(snap, "a.one")
	if _, ok := c.Tool("a.one"); !ok {
		t.Fatal("changing a snapshot must not change the catalog")
	}
	ints := c.Integrations()
	delete(ints, "a")
	idents := c.Identities()
	delete(idents, "id_a")
	if _, ok := c.Integration("a"); !ok {
		t.Fatal("integrations snapshot")
	}
	if _, ok := c.Identity("id_a"); !ok {
		t.Fatal("identities snapshot")
	}
}

func TestCatalog_ConcurrentUse(t *testing.T) {
	c := domain.NewCatalog()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(3)
		go func(i int) { defer wg.Done(); _ = c.AddSource(source(fmt.Sprintf("s%d", i%5), "", "a", "b")) }(i)
		go func() { defer wg.Done(); _ = c.Tools(); _ = c.Identities(); c.ToolsOf("s1") }()
		go func(i int) {
			defer wg.Done()
			c.RemoveSource(fmt.Sprintf("s%d", i%5))
			c.PutIdentity(&domain.Identity{ID: fmt.Sprint(i)})
		}(i)
	}
	wg.Wait()
}

func TestVisibleTo(t *testing.T) {
	platform := &domain.Integration{ID: "p"}
	mine := &domain.Integration{ID: "m", CreatedBy: "me"}
	if !domain.VisibleTo(platform, "anyone") || !domain.VisibleTo(mine, "me") || domain.VisibleTo(mine, "someone-else") || domain.VisibleTo(mine, "") {
		t.Fatal("platform integrations are shared; a creator's own belong to them")
	}
}

func TestCatalog_HydrateSkillSeesWhatIsRegisteredNow(t *testing.T) {
	c := domain.NewCatalog()
	execs := domain.NewExecutorRegistry()
	execs.Register("a", echoExecutor{})
	skill := &domain.Skill{ID: "s", Integrations: []string{"a"}}

	if _, err := c.HydrateSkill(skill, nil, execs); err == nil {
		t.Fatal("an integration that isn't registered yet can't be hydrated")
	}
	_ = c.AddSource(source("a", "", "one"))
	got, err := c.HydrateSkill(skill, nil, execs)
	if err != nil || len(got) != 1 {
		t.Fatalf("after registering: %v %v", got, err)
	}
	// Added later, seen on the very next hydration: no restart, no second list.
	_ = c.AddSource(source("a", "", "one", "two"))
	if got, _ := c.HydrateSkill(skill, nil, execs); len(got) != 2 {
		t.Fatalf("a tool added later is seen at once: %d", len(got))
	}
}
