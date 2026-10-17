package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func TestPendingTurn_SavedReplacedTakenOnceAndExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveUser(ctx, &domain.User{ID: "u_pt"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(ctx, "u_pt") })

	conv := domain.ConversationRef{Provider: "telegram", ChatID: "123", IdentityID: "id_tg", ThreadID: "9"}
	save := func(msg string) {
		t.Helper()
		if err := s.SavePendingTurn(ctx, &domain.PendingTurn{UserID: "u_pt", IdentityID: "id_aegis", AgentID: "a1", Conv: conv, Message: msg}); err != nil {
			t.Fatal(err)
		}
	}

	save("buy 10000 EURUSD")
	save("buy 20000 EURUSD") // a newer request replaces the older one
	got, err := s.TakePendingTurn(ctx, "u_pt", "id_aegis", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message != "buy 20000 EURUSD" || got.AgentID != "a1" || got.Conv != conv {
		t.Errorf("what was kept: %+v", got)
	}
	if _, err := s.TakePendingTurn(ctx, "u_pt", "id_aegis", time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("taken once only: %v", err)
	}

	// An old instruction is never carried out as if it were fresh.
	save("buy 30000 EURUSD")
	time.Sleep(30 * time.Millisecond)
	if _, err := s.TakePendingTurn(ctx, "u_pt", "id_aegis", 10*time.Millisecond); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an expired request must not come back: %v", err)
	}
	if _, err := s.TakePendingTurn(ctx, "u_pt", "id_aegis", time.Hour); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("and it is gone, not left to be taken later: %v", err)
	}
}

func TestUserSettings_TimezoneAndName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveUser(ctx, &domain.User{ID: "u_tz", FirstName: "Old"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(ctx, "u_tz") })

	if tz, err := s.UserTimezone(ctx, "u_tz"); err != nil || tz != "" {
		t.Fatalf("unset time zone is empty: %q %v", tz, err)
	}
	if err := s.SetUserTimezone(ctx, "u_tz", "Africa/Lagos"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserName(ctx, "u_tz", "Ada", "Obi"); err != nil {
		t.Fatal(err)
	}
	if tz, _ := s.UserTimezone(ctx, "u_tz"); tz != "Africa/Lagos" {
		t.Errorf("time zone: %q", tz)
	}
	u, err := s.GetUser(ctx, "u_tz")
	if err != nil || u.FirstName != "Ada" || u.LastName != "Obi" {
		t.Errorf("name: %+v %v", u, err)
	}
	// Saving the user again (as other flows do) must not wipe the time zone.
	if err := s.SaveUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if tz, _ := s.UserTimezone(ctx, "u_tz"); tz != "Africa/Lagos" {
		t.Errorf("a later SaveUser wiped the time zone: %q", tz)
	}
	if err := s.SetUserTimezone(ctx, "nobody", "Europe/London"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
	if _, err := s.UserTimezone(ctx, "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
}
