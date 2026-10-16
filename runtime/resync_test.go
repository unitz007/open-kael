package runtime

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func evSkill(id, event string) *domain.Skill {
	return &domain.Skill{ID: id, Name: id, Trigger: &domain.Trigger{Type: domain.TriggerTypeEvent, Value: event}}
}

func cronSkill(id, spec string) *domain.Skill {
	return &domain.Skill{ID: id, Name: id, Trigger: &domain.Trigger{Type: domain.TriggerTypeCron, Value: spec}}
}

func hostedWith(id string, skills ...*domain.Skill) *HostedAgent {
	return &HostedAgent{Agent: &domain.Agent{ID: id, Skills: skills}}
}

func reloadTo(h *Host, agentID string, skills ...*domain.Skill) {
	h.ReloadAgent(context.Background(), agentID, func(context.Context, string) (*domain.Agent, error) {
		return &domain.Agent{ID: agentID, Skills: skills}, nil
	})
}

func subCount(h *Host, event string) int {
	h.bus.mu.RLock()
	defer h.bus.mu.RUnlock()
	return len(h.bus.subs[event])
}

func cronCount(h *Host) int {
	h.cronMu.Lock()
	defer h.cronMu.Unlock()
	if h.sched == nil {
		return -1
	}
	return len(h.sched.Entries())
}

const never = "0 0 1 1 *" // 1 January 00:00 — never fires during a test

func TestEventSubscriptions_FollowSkillChanges(t *testing.T) {
	h := NewHost()
	h.Register(hostedWith("a1", evSkill("s1", "github.push")))
	if subCount(h, "github.push") != 1 {
		t.Fatalf("a registered agent's event skill should be subscribed, got %d", subCount(h, "github.push"))
	}

	// Delete the skill: it must stop being subscribed.
	reloadTo(h, "a1")
	if subCount(h, "github.push") != 0 {
		t.Errorf("a deleted skill must stop receiving events, still %d subscribed", subCount(h, "github.push"))
	}

	// Create one after startup: it starts without a restart.
	reloadTo(h, "a1", evSkill("s2", "stripe.paid"))
	if subCount(h, "stripe.paid") != 1 {
		t.Errorf("a new event skill should subscribe immediately, got %d", subCount(h, "stripe.paid"))
	}

	// Edit its trigger: it moves to the new event.
	reloadTo(h, "a1", evSkill("s2", "stripe.refunded"))
	if subCount(h, "stripe.paid") != 0 || subCount(h, "stripe.refunded") != 1 {
		t.Errorf("an edited trigger should move: paid=%d refunded=%d", subCount(h, "stripe.paid"), subCount(h, "stripe.refunded"))
	}

	// Reloading repeatedly never duplicates subscriptions.
	for i := 0; i < 3; i++ {
		reloadTo(h, "a1", evSkill("s2", "stripe.refunded"))
	}
	if subCount(h, "stripe.refunded") != 1 {
		t.Errorf("repeated reloads must not duplicate subscriptions, got %d", subCount(h, "stripe.refunded"))
	}
}

func TestEventSubscriptions_AgentsAreIndependent(t *testing.T) {
	h := NewHost()
	h.Register(hostedWith("a1", evSkill("s1", "e")))
	h.Register(hostedWith("a2", evSkill("s2", "e")))
	if subCount(h, "e") != 2 {
		t.Fatalf("both agents should be subscribed, got %d", subCount(h, "e"))
	}
	reloadTo(h, "a1") // a1 loses its skill; a2 is untouched
	if subCount(h, "e") != 1 {
		t.Errorf("only a1's subscription should go, %d left", subCount(h, "e"))
	}
	h.Deregister("a2", nil)
	if subCount(h, "e") != 0 {
		t.Errorf("a removed agent must stop receiving events, %d left", subCount(h, "e"))
	}
}

func TestCron_StartsWithExistingAgentsAndFollowsChanges(t *testing.T) {
	h := NewHost()
	h.Register(hostedWith("a1", cronSkill("c1", never), cronSkill("c2", never)))
	if cronCount(h) != -1 {
		t.Fatalf("nothing is scheduled before StartCron")
	}
	c, err := h.StartCron(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if cronCount(h) != 2 {
		t.Fatalf("StartCron should schedule the existing agent's two skills, got %d", cronCount(h))
	}

	reloadTo(h, "a1", cronSkill("c1", never)) // c2 deleted
	if cronCount(h) != 1 {
		t.Errorf("a deleted cron skill must stop firing, %d scheduled", cronCount(h))
	}
	reloadTo(h, "a1", cronSkill("c1", never), cronSkill("c3", never)) // c3 created
	if cronCount(h) != 2 {
		t.Errorf("a new cron skill should be scheduled without a restart, %d scheduled", cronCount(h))
	}
	h.Register(hostedWith("a2", cronSkill("d1", never)))
	if cronCount(h) != 3 {
		t.Errorf("a newly registered agent's cron skills should be scheduled, %d scheduled", cronCount(h))
	}
	h.Deregister("a1", nil)
	if cronCount(h) != 1 {
		t.Errorf("a removed agent's cron skills must stop, %d scheduled", cronCount(h))
	}
}

func TestCron_InvalidExpressionIsSkippedNotFatal(t *testing.T) {
	h := NewHost()
	h.Register(hostedWith("a1", cronSkill("bad", "not a cron"), cronSkill("good", never)))
	c, err := h.StartCron(context.Background())
	if err != nil {
		t.Fatalf("one bad expression must not stop the scheduler: %v", err)
	}
	defer c.Stop()
	if cronCount(h) != 1 {
		t.Errorf("only the valid skill should be scheduled, got %d", cronCount(h))
	}
	reloadTo(h, "a1", cronSkill("bad", "still bad"), cronSkill("good", never))
	if cronCount(h) != 1 {
		t.Errorf("a bad edit must not drop the good skill, got %d", cronCount(h))
	}
}

func TestReloadAgent_UnknownAgentIsIgnored(t *testing.T) {
	h := NewHost()
	reloadTo(h, "ghost", evSkill("s", "e"))
	if subCount(h, "e") != 0 {
		t.Errorf("reloading an unregistered agent must not subscribe anything")
	}
}
