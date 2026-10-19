package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func newProposal(id, person string, expires time.Time) *domain.Proposal {
	return &domain.Proposal{
		ID: id, PersonID: person, AgentID: "a1", IdentityID: "id1", ChatID: "chat1", MessageID: "77",
		Text: "Your dentist overlaps your standup.", ExpiresAt: expires,
		Choices: []domain.ProposalChoice{{Label: "Move dentist", Say: "Move my dentist to 10:15"}, {Label: "Keep both", Say: "Keep both"}, {Label: "Move standup", Say: "Move my standup to 10:15"}},
	}
}

func TestProposals_SavedAndChosenOnceAndOnlyOnce(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	now := time.Now()
	if err := s.SaveProposal(ctx, newProposal("pr1", "p1", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProposal(ctx, "pr1")
	if err != nil || got.Text != "Your dentist overlaps your standup." || len(got.Choices) != 3 || got.Choices[1].Label != "Keep both" || got.Chosen != nil || got.CreatedAt.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetProposal(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("%v", err)
	}

	chosen, err := s.ChooseProposal(ctx, "pr1", 1, now)
	if err != nil || chosen.Chosen == nil || *chosen.Chosen != 1 || chosen.Choices[1].Say != "Keep both" {
		t.Fatalf("the tap returns what was chosen: %+v %v", chosen, err)
	}
	if _, err := s.ChooseProposal(ctx, "pr1", 0, now); !errors.Is(err, domain.ErrProposalChosen) {
		t.Errorf("a second tap, on any button, changes nothing: %v", err)
	}
	if _, err := s.ChooseProposal(ctx, "pr1", 1, now); !errors.Is(err, domain.ErrProposalChosen) {
		t.Errorf("not even the same button again: %v", err)
	}
	if after, _ := s.GetProposal(ctx, "pr1"); after.Chosen == nil || *after.Chosen != 1 || after.ChosenAt == nil {
		t.Errorf("the first tap stands: %+v", after)
	}
}

func TestProposals_ExpiredAndUnknownPositions(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	now := time.Now()
	_ = s.SaveProposal(ctx, newProposal("old", "p1", now.Add(-time.Minute)))
	_ = s.SaveProposal(ctx, newProposal("fresh", "p1", now.Add(time.Hour)))

	if _, err := s.ChooseProposal(ctx, "old", 0, now); !errors.Is(err, domain.ErrProposalExpired) {
		t.Errorf("buttons stop working: %v", err)
	}
	if p, _ := s.GetProposal(ctx, "old"); p.Chosen != nil {
		t.Error("and an expired tap chooses nothing")
	}
	for _, i := range []int{-1, 3, 99} {
		if _, err := s.ChooseProposal(ctx, "fresh", i, now); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("button %d does not exist: %v", i, err)
		}
	}
	if p, _ := s.GetProposal(ctx, "fresh"); p.Chosen != nil {
		t.Error("a bad position chooses nothing, and the real buttons still work")
	}
	if _, err := s.ChooseProposal(ctx, "missing", 0, now); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("%v", err)
	}

	// Cleaning up removes only what expired.
	n, err := s.DeleteExpiredProposals(ctx, now)
	if err != nil || n != 1 {
		t.Errorf("%d %v", n, err)
	}
	if _, err := s.GetProposal(ctx, "old"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("the expired one is gone")
	}
	if _, err := s.GetProposal(ctx, "fresh"); err != nil {
		t.Error("the live one stays")
	}
}

func TestProposals_ManyTapsAtOnceChooseExactlyOne(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	_ = s.SaveProposal(ctx, newProposal("race", "p1", time.Now().Add(time.Hour)))

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, closed := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.ChooseProposal(ctx, "race", i%3, time.Now())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, domain.ErrProposalChosen):
				closed++
			default:
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || closed != 19 {
		t.Errorf("exactly one tap acts, however many arrive together: %d won, %d turned away", wins, closed)
	}
}

func TestProposals_FollowThePersonThroughAMergeAndGoWithThem(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "gone")
	person(t, s, "stays")
	_ = s.SaveProposal(ctx, newProposal("pr_gone", "gone", time.Now().Add(time.Hour)))
	if err := s.MergePersons(ctx, "gone", "stays"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.GetProposal(ctx, "pr_gone"); err != nil || p.PersonID != "stays" {
		t.Errorf("buttons already sent now belong to the person who remains: %+v %v", p, err)
	}
	if err := s.DeletePerson(ctx, "stays"); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM proposals`) != 0 {
		t.Error("deleting a person removes their proposals")
	}
}
