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

func TestWelcomeMessage_RememberedThenTakenOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveUser(ctx, &domain.User{ID: "u_wm"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIntegration(ctx, &domain.Integration{ID: "int_wm", Name: "Telegram", Service: "telegram"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity(ctx, &domain.Identity{ID: "id_wm", IntegrationID: "int_wm", Name: "Bot", Kind: "bot"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessengerChannel(ctx, &domain.MessengerChannel{ID: "mc_wm", IdentityID: "id_wm", UserID: "u_wm", ChannelRef: "555"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(ctx, "u_wm") })

	if id, err := s.TakeWelcomeMessage(ctx, "id_wm", "555"); err != nil || id != "" {
		t.Fatalf("nothing remembered yet: %q %v", id, err)
	}
	if err := s.SaveWelcomeMessage(ctx, "id_wm", "555", "9001"); err != nil {
		t.Fatal(err)
	}
	if id, err := s.TakeWelcomeMessage(ctx, "id_wm", "555"); err != nil || id != "9001" {
		t.Fatalf("remembered: %q %v", id, err)
	}
	if id, err := s.TakeWelcomeMessage(ctx, "id_wm", "555"); err != nil || id != "" {
		t.Errorf("taken once only: %q %v", id, err)
	}
	if id, err := s.TakeWelcomeMessage(ctx, "id_wm", "no-such-chat"); err != nil || id != "" {
		t.Errorf("an unknown chat is simply empty: %q %v", id, err)
	}
	// Saving the channel again (as the listener does) must not forget the remembered message.
	_ = s.SaveWelcomeMessage(ctx, "id_wm", "555", "9002")
	_ = s.SaveMessengerChannel(ctx, &domain.MessengerChannel{ID: "mc_wm", IdentityID: "id_wm", UserID: "u_wm", ChannelRef: "555"})
	if id, _ := s.TakeWelcomeMessage(ctx, "id_wm", "555"); id != "9002" {
		t.Errorf("re-saving the channel wiped the remembered message: %q", id)
	}
}

func TestUserDetails_LanguageResidenceAndNameAreKept(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SaveUser(ctx, &domain.User{ID: "u_ud", FirstName: "Ada"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeleteUser(ctx, "u_ud") })

	if d, err := s.UserDetails(ctx, "u_ud"); err != nil || d != (domain.UserDetails{Name: "Ada"}) {
		t.Fatalf("only the name so far: %+v %v", d, err)
	}
	if err := s.SetUserTimezone(ctx, "u_ud", "Africa/Lagos"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserLanguage(ctx, "u_ud", "fr"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserResidence(ctx, "u_ud", "NG", "Lagos"); err != nil {
		t.Fatal(err)
	}
	want := domain.UserDetails{Name: "Ada", Timezone: "Africa/Lagos", Language: "fr", Country: "NG", City: "Lagos"}
	if d, err := s.UserDetails(ctx, "u_ud"); err != nil || d != want {
		t.Fatalf("all of it: %+v %v", d, err)
	}
	// Another flow saving the user must not wipe any of it.
	u, _ := s.GetUser(ctx, "u_ud")
	if err := s.SaveUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.UserDetails(ctx, "u_ud"); d != want {
		t.Errorf("a later SaveUser wiped something: %+v", d)
	}
	if _, err := s.UserDetails(ctx, "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
	if err := s.SetUserLanguage(ctx, "nobody", "fr"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
	if err := s.SetUserResidence(ctx, "nobody", "NG", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
}
