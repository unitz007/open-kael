package postgres_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A skill an agent learned in its chats is kept with its steps and whose it is, and never read back as one of its
// capabilities: routing, what it may use and readiness only ever see those.
func TestLearnedSkills_KeptApartFromCapabilities(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	if err := s.SaveAgent(ctx, &domain.Agent{ID: "a1", Name: "Ledger"}); err != nil {
		t.Fatal(err)
	}
	own := &domain.Skill{ID: "apps-a1", AgentID: "a1", Name: "its_apps"}
	everyone := &domain.Skill{ID: "l1", AgentID: "a1", Name: "chasing-a-late-payer", Description: "When asked to chase someone who hasn't paid",
		Instructions: "1. Check what they owe", Origin: domain.SkillLearned,
		Steps: []domain.SkillStep{{Text: "Check what they owe", Ability: "pay_list"}, {Text: "Leave it as a draft"}}}
	theirs := &domain.Skill{ID: "l2", AgentID: "a1", Name: "daily-cash-up", Origin: domain.SkillLearned, PersonID: "p1",
		Steps: []domain.SkillStep{{Text: "Total today's payments", Ability: "pay_list"}}}
	for _, sk := range []*domain.Skill{own, everyone, theirs} {
		if err := s.SaveSkill(ctx, sk); err != nil {
			t.Fatal(err)
		}
	}

	caps, err := s.ListSkillsByAgent(ctx, "a1")
	if err != nil || len(caps) != 1 || caps[0].ID != "apps-a1" {
		t.Fatalf("only its own capability: %+v %v", caps, err)
	}
	byAgent, err := s.ListSkillsByAgents(ctx, []string{"a1"})
	if err != nil || len(byAgent["a1"]) != 1 {
		t.Fatalf("nor in the agent list: %+v %v", byAgent, err)
	}
	learned, err := s.ListLearnedSkills(ctx, "a1")
	if err != nil || len(learned) != 2 {
		t.Fatalf("both learned: %+v %v", learned, err)
	}
	if got := learned[0]; got.Origin != domain.SkillLearned || got.PersonID != "" || !reflect.DeepEqual(got.Steps, everyone.Steps) {
		t.Errorf("everyone's, with its steps: %+v", got)
	}
	if got := learned[1]; got.PersonID != "p1" {
		t.Errorf("a person's own: %+v", got)
	}
	agent, err := s.LoadAgent(ctx, "a1")
	if err != nil || len(agent.Skills) != 1 || len(agent.Learned) != 2 {
		t.Fatalf("a loaded agent has them apart: %+v %v", agent, err)
	}
	if got, err := s.GetSkill(ctx, "apps-a1"); err != nil || got.Origin != "" || got.Steps != nil {
		t.Errorf("a capability is read back as one: %+v %v", got, err)
	}

	// A person's own move with them when two people turn out to be one, and go when they do.
	person(t, s, "p2")
	if err := s.MergePersons(ctx, "p1", "p2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSkill(ctx, "l2"); got.PersonID != "p2" {
		t.Errorf("moved on a merge: %+v", got)
	}
	exec(t, pool, `DELETE FROM persons WHERE id = 'p2'`)
	if _, err := s.GetSkill(ctx, "l2"); err != domain.ErrNotFound {
		t.Errorf("gone with the person: %v", err)
	}
	if _, err := s.GetSkill(ctx, "l1"); err != nil {
		t.Errorf("everyone's stays: %v", err)
	}
}
