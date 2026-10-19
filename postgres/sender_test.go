package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// botWorld is the world plus three Telegram bots and two Slack apps.
func botWorld(t *testing.T) (*postgres.Store, func(sql string, args ...any)) {
	t.Helper()
	s, pool := world(t)
	run := func(sql string, args ...any) { exec(t, pool, sql, args...) }
	run(`INSERT INTO integrations (id, name, service) VALUES ('slack', 'Slack', 'slack')`)
	run(`INSERT INTO identities (id, integration_id) VALUES ('tg1', 'int1'), ('tg2', 'int1'), ('tg3', 'int1'), ('sl1', 'slack'), ('sl2', 'slack')`)
	return s, run
}

func channel(run func(string, ...any), id, identity, person, ref, sender string) {
	run(`INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref, sender_id) VALUES ($1, $2, $3, $4, $5)`, id, identity, person, ref, sender)
}

func TestFindPersonBySender(t *testing.T) {
	s, run := botWorld(t)
	ctx := context.Background()
	for _, p := range []string{"ada", "bob", "cy", "old"} {
		person(t, s, p)
	}
	channel(run, "c0", "tg1", "old", "50", "") // an older channel that never learned its sender
	channel(run, "c1", "tg1", "ada", "100", "tgsender1")
	channel(run, "c2", "sl1", "bob", "C1", "U1")

	find := func(identity, sender string) string {
		got, err := s.FindPersonBySender(ctx, identity, sender)
		if err != nil {
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("%s %s: %v", identity, sender, err)
			}
			return ""
		}
		return got
	}
	if find("tg1", "tgsender1") != "ada" {
		t.Error("the same bot")
	}
	if find("tg2", "tgsender1") != "ada" || find("tg3", "tgsender1") != "ada" {
		t.Error("the same Telegram user writing to another Telegram bot is the same person")
	}
	if find("sl1", "U1") != "bob" {
		t.Error("slack on the same app")
	}
	if find("sl2", "U1") != "" {
		t.Error("a Slack id means something only inside one workspace, so another app is not assumed to be the same person")
	}
	if find("sl1", "tgsender1") != "" || find("tg1", "U1") != "" {
		t.Error("one messenger's ids are never taken for another's")
	}
	if find("tg1", "") != "" || find("tg1", "nobody") != "" {
		t.Error("nobody, and nobody known")
	}

	// Two people for one sender (before this was recognised): the one on this very bot wins.
	channel(run, "c3", "tg2", "cy", "200", "tgsender1")
	if find("tg2", "tgsender1") != "cy" {
		t.Error("this bot's own record comes first")
	}
	if find("tg3", "tgsender1") != "ada" {
		t.Error("otherwise the one seen first")
	}
}

func TestDuplicatePeople_FindsOneHumanAcrossBotsAndChoosesWhoStays(t *testing.T) {
	s, run := botWorld(t)
	ctx := context.Background()
	for _, p := range []string{"a", "b", "c", "d", "g", "h", "lone", "slack1", "slack2", "e", "f"} {
		person(t, s, p)
	}
	// a, b and c are one human: a and b share a Telegram account, b and c share another.
	channel(run, "ca", "tg1", "a", "1", "s1")
	channel(run, "cb1", "tg2", "b", "2", "s1")
	channel(run, "cb2", "tg2", "b", "3", "s2")
	channel(run, "cc", "tg3", "c", "4", "s2")
	// c has a verified email, so c stays although a has said more.
	run(`INSERT INTO accounts (id, email, email_verified, person_id) VALUES ('acc_c', 'c@example.com', TRUE, 'c')`)
	for i := 0; i < 3; i++ {
		run(`INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'a', 'user')`)
	}
	run(`INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'c', 'user')`)
	// d, g and h: g has said the most.
	channel(run, "cg", "tg1", "g", "5", "s3")
	channel(run, "ch", "tg2", "h", "6", "s3")
	channel(run, "cd", "tg3", "d", "7", "s3")
	for i := 0; i < 5; i++ {
		run(`INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'g', 'user')`)
	}
	run(`INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'h', 'user'), ('a1', 'h', 'user')`)
	// e and f: nothing to tell them apart, so the lowest id stays.
	channel(run, "ce", "tg1", "e", "8", "s4")
	channel(run, "cf", "tg2", "f", "9", "s4")
	// Slack: the same id on two apps is not enough. And someone on one bot only is not a duplicate.
	channel(run, "csl1", "sl1", "slack1", "C1", "U9")
	channel(run, "csl2", "sl2", "slack2", "C2", "U9")
	channel(run, "cl", "tg1", "lone", "10", "s5")

	groups, err := s.DuplicatePeople(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("three humans were split: %+v", groups)
	}
	survivor := map[string]string{}
	for _, g := range groups {
		for _, m := range g.Members {
			survivor[m.PersonID] = g.Survivor
		}
		if len(g.Via) != 1 || g.Via[0] != "telegram" {
			t.Errorf("found through Telegram: %v", g.Via)
		}
	}
	for p, want := range map[string]string{"a": "c", "b": "c", "c": "c", "d": "g", "g": "g", "h": "g", "e": "e", "f": "e"} {
		if survivor[p] != want {
			t.Errorf("%s ends up as %s, want %s", p, survivor[p], want)
		}
	}
	for _, p := range []string{"lone", "slack1", "slack2"} {
		if _, in := survivor[p]; in {
			t.Errorf("%s is not a duplicate", p)
		}
	}

	// Merging puts each human together, and nothing is left to find.
	for _, g := range groups {
		if err := s.MergeDuplicates(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, s, "c"); n != 4 {
		t.Errorf("c now has every channel of the three: %d", n)
	}
	if again, _ := s.DuplicatePeople(ctx); len(again) != 0 {
		t.Errorf("nothing left to merge: %+v", again)
	}
	if got, _ := s.GetPerson(ctx, "a"); got.ID != "c" {
		t.Errorf("old ids lead to the person who stays: %+v", got)
	}
	// And a new message from the same account on any of the bots now finds that one person.
	if got, _ := s.FindPersonBySender(ctx, "tg2", "s1"); got != "c" {
		t.Errorf("%q", got)
	}
}

func countRows(t *testing.T, s *postgres.Store, person string) int {
	t.Helper()
	chs, err := s.ListMessengerChannelsByUser(context.Background(), person)
	if err != nil {
		t.Fatal(err)
	}
	return len(chs)
}

func TestMessengerChannel_EmailLinkStateComesBackFromEveryLookup(t *testing.T) {
	s, run := botWorld(t)
	ctx := context.Background()
	person(t, s, "ada")
	channel(run, "c1", "tg1", "ada", "100", "sender1")

	// Nothing set yet.
	ch, err := s.GetMessengerChannelByIdentityAndRef(ctx, "tg1", "100")
	if err != nil || ch.EmailLinkState != "" || ch.EmailLinkStateAt != nil {
		t.Fatalf("%+v %v", ch, err)
	}

	before := time.Now().Add(-2 * time.Second)
	if err := s.SetMessengerChannelEmailLinkState(ctx, "tg1", "100", "awaiting"); err != nil {
		t.Fatal(err)
	}
	byRef, _ := s.GetMessengerChannelByIdentityAndRef(ctx, "tg1", "100")
	bySender, _ := s.GetMessengerChannelByIdentityAndSender(ctx, "tg1", "sender1")
	list, _ := s.ListMessengerChannelsByUser(ctx, "ada")
	if len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	for name, got := range map[string]*domain.MessengerChannel{"by chat": byRef, "by sender": bySender, "in the list": list[0]} {
		if got.EmailLinkState != "awaiting" {
			t.Errorf("%s: the state is read back (it never was): %q", name, got.EmailLinkState)
		}
		if got.EmailLinkStateAt == nil || got.EmailLinkStateAt.Before(before) || got.EmailLinkStateAt.After(time.Now().Add(time.Minute)) {
			t.Errorf("%s: and when it was set: %v", name, got.EmailLinkStateAt)
		}
		if got.EffectiveEmailLinkState(time.Now()) != "awaiting" {
			t.Errorf("%s: a fresh wait counts", name)
		}
	}

	// Saving the channel again (every message resolves it) leaves the state alone.
	if err := s.SaveMessengerChannel(ctx, &domain.MessengerChannel{ID: "c1", IdentityID: "tg1", UserID: "ada", ChannelRef: "100", SenderID: "sender1"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetMessengerChannelByIdentityAndRef(ctx, "tg1", "100"); again.EmailLinkState != "awaiting" {
		t.Errorf("saving the channel must not forget the wait: %q", again.EmailLinkState)
	}

	// An old wait, as the database would hold it after the person wandered off, no longer counts.
	run(`UPDATE messenger_channels SET email_link_state_at = now() - interval '2 hours' WHERE id = 'c1'`)
	old, _ := s.GetMessengerChannelByIdentityAndRef(ctx, "tg1", "100")
	if old.EffectiveEmailLinkState(time.Now()) != "" {
		t.Error("a wait from hours ago does not capture the next message")
	}
	// A row from before the time was kept.
	run(`UPDATE messenger_channels SET email_link_state_at = NULL WHERE id = 'c1'`)
	legacy, _ := s.GetMessengerChannelByIdentityAndRef(ctx, "tg1", "100")
	if legacy.EmailLinkStateAt != nil || legacy.EffectiveEmailLinkState(time.Now()) != "" {
		t.Errorf("%+v", legacy)
	}
}
