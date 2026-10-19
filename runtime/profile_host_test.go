package runtime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func profileHosted() *HostedAgent {
	return &HostedAgent{
		Agent: &domain.Agent{
			ID: "a1", CreatedBy: "creator",
			Skills:      []*domain.Skill{{ID: "s1", Tools: []domain.ToolBinding{{ToolID: "t_squad"}}}, {ID: "s2", Integrations: []string{"int_cal"}}},
			IdentityIDs: []string{"id_tg"},
		},
		Deps: AgentDeps{
			IntegrationsByID: map[string]*domain.Integration{
				"int_fpl":   {ID: "int_fpl", Name: "Fantasy Football"},
				"int_cal":   {ID: "int_cal", Name: "Calendar"},
				"int_tg":    {ID: "int_tg", Name: "Telegram"},
				"int_trade": {ID: "int_trade", Name: "Trading"}, // visible to the creator, but this agent does not use it
			},
			IdentitiesByID: map[string]*domain.Identity{"id_tg": {ID: "id_tg", IntegrationID: "int_tg"}},
			ToolsByID:      map[string]*domain.ToolDefinition{"t_squad": {ID: "t_squad", IntegrationID: "int_fpl"}},
		},
	}
}

func TestUsedIntegrations_OnlyWhatTheAgentWorksWith(t *testing.T) {
	got := profileHosted().usedIntegrations()
	if len(got) != 3 || got["int_fpl"] == nil || got["int_cal"] == nil || got["int_tg"] == nil || got["int_trade"] != nil {
		t.Errorf("by a tool, by a grant and by an account, but not the rest: %v", got)
	}
}

func TestProfileForPrompt_MapsAndNoLeakAcrossAgents(t *testing.T) {
	st := newMemStore()
	st.seed(P, "name", "Charles")
	st.seed(P, "residence", "Wolfsburg, Germany")
	st.seed("int_fpl", "favourite_club", "Arsenal")
	st.seed("int_trade", "risk", "careful") // shared with an app this agent does not use
	h := NewHost()
	h.SetProfileDetails(st, nil)

	personal, apps := h.profileForPrompt(context.Background(), "u1", profileHosted())
	if personal != "name: Charles\nresidence: Wolfsburg, Germany" {
		t.Errorf("personal map as lines: %q", personal)
	}
	if apps["int_fpl"] != "favourite_club: Arsenal" {
		t.Errorf("its own app: %v", apps)
	}
	if _, leaked := apps["int_trade"]; leaked {
		t.Errorf("what a person shared with another app must not reach this agent: %v", apps)
	}
	if p, a := h.profileForPrompt(context.Background(), "", profileHosted()); p != "" || a != nil {
		t.Error("no person, nothing")
	}
}

func TestProfileForPrompt_TypedDetailsAppearInTheSameMap(t *testing.T) {
	st := newMemStore()
	st.seed(P, "hobbies", "chess")
	h := NewHost()
	h.SetProfileDetails(st, &fakeTyped{vals: map[string]string{"name": "Charles", "time_zone": "Europe/Berlin"}})
	personal, _ := h.profileForPrompt(context.Background(), "u1", profileHosted())
	if personal != "hobbies: chess\nname: Charles\ntime_zone: Europe/Berlin" {
		t.Errorf("%q", personal)
	}
}

type recMigrator struct {
	mu    sync.Mutex
	calls []string
	block chan struct{}
	n     atomic.Int32
}

func (r *recMigrator) Migrate(_ context.Context, userID, scope, legacy string) error {
	r.n.Add(1)
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.calls = append(r.calls, userID+"|"+scope+"|"+legacy)
	r.mu.Unlock()
	return nil
}

func TestProfileForPrompt_OlderNoteStillShownUntilMoved_AndMovedOnce(t *testing.T) {
	st := newMemStore()
	st.seed(P, "name", "Charles")
	h := NewHost()
	h.SetProfileDetails(st, nil)
	mig := &recMigrator{block: make(chan struct{})}
	h.SetProfileMigrator(mig)
	h.SetUserProfileLoader(func(context.Context, string) (*domain.UserProfile, error) {
		return &domain.UserProfile{UserID: "u1", Notes: "Supports Arsenal, lives in Wolfsburg."}, nil
	})
	h.SetUserIntegrationNotesLoader(func(context.Context, string) (map[string]string, error) {
		return map[string]string{"int_fpl": "Team: Gunners FC", "int_trade": "Risk: careful"}, nil
	})

	personal, apps := h.profileForPrompt(context.Background(), "u1", profileHosted())
	if !strings.Contains(personal, "name: Charles") || !strings.Contains(personal, "Supports Arsenal, lives in Wolfsburg.") {
		t.Errorf("until it is moved the older note still counts: %q", personal)
	}
	if apps["int_fpl"] != "Team: Gunners FC" {
		t.Errorf("%v", apps)
	}
	if _, leaked := apps["int_trade"]; leaked {
		t.Errorf("older notes of other apps must not leak either: %v", apps)
	}
	// A second turn while the move is still running does not start another.
	h.profileForPrompt(context.Background(), "u1", profileHosted())
	time.Sleep(50 * time.Millisecond)
	if mig.n.Load() != 2 {
		t.Errorf("one move per note (personal and Fantasy Football), however many turns: %d", mig.n.Load())
	}
	close(mig.block)

	// Once moved, the older note is no longer shown.
	st.migrated[P] = true
	st.migrated["int_fpl"] = true
	personal, apps = h.profileForPrompt(context.Background(), "u1", profileHosted())
	if strings.Contains(personal, "Supports Arsenal") || apps["int_fpl"] != "" {
		t.Errorf("moved notes are not shown twice: %q %v", personal, apps)
	}
}

func TestProfileForPrompt_WithoutDetailStoreKeepsOlderBehaviourButNoLeak(t *testing.T) {
	h := NewHost()
	h.SetUserProfileLoader(func(context.Context, string) (*domain.UserProfile, error) {
		return &domain.UserProfile{Notes: "likes tea"}, nil
	})
	h.SetUserIntegrationNotesLoader(func(context.Context, string) (map[string]string, error) {
		return map[string]string{"int_fpl": "Team: Gunners FC", "int_trade": "Risk: careful"}, nil
	})
	personal, apps := h.profileForPrompt(context.Background(), "u1", profileHosted())
	if personal != "likes tea" || apps["int_fpl"] == "" || apps["int_trade"] != "" {
		t.Errorf("%q %v", personal, apps)
	}
}

type recLearner struct {
	reqs chan LearnRequest
	ctx  chan context.Context
}

func (r *recLearner) Learn(ctx context.Context, req LearnRequest) error {
	r.ctx <- ctx
	r.reqs <- req
	return nil
}

type noButtons struct{}

func (noButtons) RequestChoice(context.Context, domain.ConversationRef, string, []string, int) (domain.ChoiceAnswer, error) {
	return domain.ChoiceAnswer{}, nil
}

func TestMaybeLearn_OnlyForRealMessagesFromKnownPeople(t *testing.T) {
	h := NewHost()
	rl := &recLearner{reqs: make(chan LearnRequest, 4), ctx: make(chan context.Context, 4)}
	h.SetDetailLearner(rl)
	hosted := profileHosted()
	conv := domain.ConversationRef{UserID: "u1", ChatID: "c"}

	h.maybeLearn(context.Background(), conv, hosted, "ok", "fine")                                                 // too short
	h.maybeLearn(context.Background(), domain.ConversationRef{ChatID: "c"}, hosted, "I live in Wolfsburg now", "") // nobody known
	select {
	case r := <-rl.reqs:
		t.Fatalf("should not have learned: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}

	turn, endTurn := context.WithCancel(context.Background())
	ctx := domain.WithChoiceRequester(turn, noButtons{})
	h.maybeLearn(ctx, conv, hosted, "I live in Wolfsburg now", "Nice!")
	endTurn() // the turn is over; learning carries on
	select {
	case r := <-rl.reqs:
		if r.UserID != "u1" || r.AgentID != "a1" || r.UserMessage != "I live in Wolfsburg now" || r.Apps["int_fpl"] != "Fantasy Football" || len(r.Apps) != 3 {
			t.Errorf("%+v", r)
		}
		lctx := <-rl.ctx
		if _, ok := domain.ChoiceRequesterFromContext(lctx); !ok {
			t.Error("the learner can ask with buttons where the conversation could")
		}
		if lctx.Err() != nil {
			t.Error("and is not cut short when the turn ends")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not learned")
	}

	h2 := NewHost() // no learner set: nothing happens, nothing breaks
	h2.maybeLearn(ctx, conv, hosted, "I live in Wolfsburg now", "Nice!")
}

type failingMigrator struct{ n atomic.Int32 }

func (f *failingMigrator) Migrate(context.Context, string, string, string) error {
	f.n.Add(1)
	return context.DeadlineExceeded
}

func TestStartMigration_AFailedMoveWaitsBeforeTryingAgain(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	old := clockNow
	clockNow = func() time.Time { return now }
	defer func() { clockNow = old }()

	h := NewHost()
	f := &failingMigrator{}
	h.SetProfileDetails(newMemStore(), nil)
	h.SetProfileMigrator(f)
	wait := func(n int32) {
		for i := 0; i < 400; i++ {
			if _, running := h.profileDetails.migrating.Load("u1|personal"); !running && f.n.Load() >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the move did not finish")
	}
	h.startMigration("u1", "personal", "note")
	wait(1)
	h.startMigration("u1", "personal", "note") // the next message, a moment later
	time.Sleep(30 * time.Millisecond)
	if f.n.Load() != 1 {
		t.Fatalf("not retried straight away: %d", f.n.Load())
	}
	now = now.Add(migrationRetry + time.Second)
	h.startMigration("u1", "personal", "note")
	wait(2)
	if f.n.Load() != 2 {
		t.Errorf("retried after the wait: %d", f.n.Load())
	}
}
