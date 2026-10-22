package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// activityRig is one bot used by two agents, with people connected through it.
func activityRig(t *testing.T) (*postgres.Store, context.Context) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveIntegration(ctx, &domain.Integration{ID: "int_tg", Name: "Telegram", Service: "telegram"}))
	must(s.SaveIntegration(ctx, &domain.Integration{ID: "int_sl", Name: "Slack", Service: "slack"}))
	must(s.SaveIdentity(ctx, &domain.Identity{ID: "id_tg", IntegrationID: "int_tg", Name: "tg bot", Kind: "bot"}))
	must(s.SaveIdentity(ctx, &domain.Identity{ID: "id_sl", IntegrationID: "int_sl", Name: "sl bot", Kind: "bot"}))
	must(s.SaveAgent(ctx, &domain.Agent{ID: "ag_a", Name: "A", MaxIterations: 5, IdentityIDs: []string{"id_tg", "id_sl"}}))
	must(s.SaveAgent(ctx, &domain.Agent{ID: "ag_b", Name: "B", MaxIterations: 5, IdentityIDs: []string{"id_sl"}}))
	for _, u := range []string{"u1", "u2", "u3"} {
		must(s.SavePerson(ctx, &domain.Person{ID: "u_act_" + u}))
		u := u
		t.Cleanup(func() { _ = s.DeletePerson(ctx, "u_act_"+u) })
	}
	_, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now()) // makes sure the secret exists
	must(err)
	_ = s.PruneActivityForTest(ctx)
	return s, ctx
}

func TestActivity_PeopleAreCountedOnceAndNeverStoredAsThemselves(t *testing.T) {
	s, ctx := activityRig(t)
	for i := 0; i < 5; i++ {
		s.PersonActive(ctx, "ag_a", "u_act_u1")
	}
	s.PersonActive(ctx, "ag_a", "u_act_u2")
	s.PersonActive(ctx, "ag_b", "u_act_u1") // the same person with another agent
	s.PersonActive(ctx, "ag_a", "")         // nobody
	s.PersonActive(ctx, "nope", "u_act_u3") // an agent that doesn't exist: ignored, not a crash

	now := time.Now()
	a, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.ActiveToday != 2 || a.ActiveWeek != 2 || a.Daily[len(a.Daily)-1].People != 2 || len(a.Daily) != 7 {
		t.Errorf("one agent: %+v", a)
	}
	both, _ := s.ActivitySummary(ctx, []string{"ag_a", "ag_b"}, 7, now)
	if both.ActiveToday != 2 {
		t.Errorf("a person with two agents is one person across them: %d", both.ActiveToday)
	}
	if rows := s.RawActivityPeopleForTest(ctx); len(rows) == 0 {
		t.Fatal("nothing was stored")
	} else {
		for _, r := range rows {
			if strings.Contains(r, "u_act_") || len(r) != 32 {
				t.Errorf("what is stored must be a one-way code, not the person: %q", r)
			}
		}
	}
}

func TestActivity_DaysAreFilledAndWeeksCountPeopleOnce(t *testing.T) {
	s, ctx := activityRig(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s.SetActivityDaysForTest(ctx, "ag_a", map[string][]string{
		"2026-10-19": {"p1", "p2"},
		"2026-10-18": {"p1"},
		"2026-10-16": {"p3"},
		"2026-10-10": {"p9"}, // outside the week
	})
	sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ActiveToday != 2 || sum.ActiveWeek != 3 {
		t.Errorf("today 2, this week 3 distinct people: %+v", sum)
	}
	got := map[string]int{}
	for _, d := range sum.Daily {
		got[d.Day] = d.People
	}
	if len(sum.Daily) != 14 || sum.Daily[0].Day != "2026-10-06" || sum.Daily[13].Day != "2026-10-19" {
		t.Errorf("14 days ending today: %v … %v", sum.Daily[0], sum.Daily[13])
	}
	if got["2026-10-19"] != 2 || got["2026-10-18"] != 1 || got["2026-10-17"] != 0 || got["2026-10-16"] != 1 || got["2026-10-10"] != 1 {
		t.Errorf("per day: %v", got)
	}
	if short, _ := s.ActivitySummary(ctx, []string{"ag_a"}, 7, now); len(short.Daily) != 7 || short.Daily[0].Day != "2026-10-13" {
		t.Errorf("7 days: %v", short.Daily)
	}
}

func TestActivity_PeopleConnectedAreCountedByAppAndNewOnesBecomeAnEvent(t *testing.T) {
	s, ctx := activityRig(t)
	save := func(id, ident, user string) {
		if err := s.SaveMessengerChannel(ctx, &domain.MessengerChannel{ID: id, IdentityID: ident, UserID: user, ChannelRef: id}); err != nil {
			t.Fatal(err)
		}
	}
	save("mc1", "id_tg", "u_act_u1")
	save("mc2", "id_sl", "u_act_u1") // the same person on a second app: one person
	save("mc3", "id_tg", "u_act_u2")
	save("mc3", "id_tg", "u_act_u2") // saved again (the listener does): not a new person, not a new event

	sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Connected != 2 || sum.ConnectedThisWeek != 2 {
		t.Errorf("two people, both new: %+v", sum)
	}
	apps := map[string]int{}
	for _, c := range sum.ChatApps {
		apps[c.Name] = c.Count
	}
	if apps["telegram"] != 2 || apps["slack"] != 1 {
		t.Errorf("people per app: %v", sum.ChatApps)
	}
	joined := 0
	for _, e := range sum.Events {
		if e.Kind == domain.ActivityJoined {
			joined++
			if e.AgentID != "ag_a" || (e.Detail != "telegram" && e.Detail != "slack") {
				t.Errorf("an event names the app and nobody else: %+v", e)
			}
		}
	}
	if joined != 3 {
		t.Errorf("3 new channels = 3 events (not 4 for the re-save): %d", joined)
	}
	// ag_b only uses Slack: it sees the Slack connection only.
	b, _ := s.ActivitySummary(ctx, []string{"ag_b"}, 7, time.Now())
	if b.Connected != 1 || len(b.ChatApps) != 1 || b.ChatApps[0].Name != "slack" {
		t.Errorf("another agent sees only its own: %+v", b)
	}
	// People who connected before this was tracked are connected but not "new this week".
	if err := s.BackdateChannelsForTest(ctx, "mc1"); err != nil {
		t.Fatal(err)
	}
	if sum, _ = s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now()); sum.Connected != 2 || sum.ConnectedThisWeek != 1 {
		t.Errorf("one is old now: %+v", sum)
	}
}

func TestActivity_EventsAreCleanedGroupedAndTopicsNeedSeveralAsks(t *testing.T) {
	s, ctx := activityRig(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.ActivityEvent(ctx, "ag_a", domain.ActivityUnsupported, "Book a flight 12345")
	}
	s.ActivityEvent(ctx, "ag_a", domain.ActivityUnsupported, "Pay Ada's bill, ada@example.com")
	s.ActivityEvent(ctx, "ag_a", domain.ActivityUnsupported, "")
	s.ActivityEvent(ctx, "ag_a", domain.ActivityFailed, "unavailable")
	s.ActivityEvent(ctx, "ag_a", domain.ActivityFailed, "unavailable")
	s.ActivityEvent(ctx, "ag_a", domain.ActivitySignIn, "Google Calendar")
	s.ActivityEvent(ctx, "ag_b", domain.ActivityFailed, "problem")

	sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Topics) != 1 || sum.Topics[0].Name != "book a flight" || sum.Topics[0].Count != 3 {
		t.Errorf("only what was asked 3+ times, cleaned of digits: %+v", sum.Topics)
	}
	for _, e := range sum.Events {
		if strings.ContainsAny(e.Detail, "0123456789@") {
			t.Errorf("a stored detail has digits or an address: %q", e.Detail)
		}
	}
	if sum.FailedToday != 2 {
		t.Errorf("failed today counts this agent only: %d", sum.FailedToday)
	}
	probs := map[string]int{}
	for _, p := range sum.Problems {
		probs[string(p.Kind)+"/"+p.Detail] = p.Count
	}
	if probs["failed/unavailable"] != 2 || probs["signin/google calendar"] != 1 || len(probs) != 2 {
		t.Errorf("problems of the last day, grouped: %v", probs)
	}
	if len(sum.Events) == 0 || sum.Events[0].ID < sum.Events[len(sum.Events)-1].ID {
		t.Errorf("newest first: %+v", sum.Events)
	}
	all, _ := s.ActivitySummary(ctx, []string{"ag_a", "ag_b"}, 14, now)
	if all.FailedToday != 3 {
		t.Errorf("all agents: %d", all.FailedToday)
	}
	none, err := s.ActivitySummary(ctx, nil, 14, now)
	if err != nil || none.Connected != 0 || len(none.Daily) != 14 || none.Events == nil || none.Topics == nil {
		t.Errorf("no agents is an empty summary, never an error or null lists: %+v %v", none, err)
	}
}

func TestActivity_OldThingsAreRemovedAndAnAgentGoingTakesItsNumbersWithIt(t *testing.T) {
	s, ctx := activityRig(t)
	s.SetActivityDaysForTest(ctx, "ag_a", map[string][]string{"2020-01-01": {"old"}, time.Now().UTC().Format("2006-01-02"): {"new"}})
	s.ActivityEvent(ctx, "ag_a", domain.ActivityFailed, "problem")
	if err := s.AgeEventsForTest(ctx, "ag_a", 40); err != nil {
		t.Fatal(err)
	}
	s.ActivityEvent(ctx, "ag_a", domain.ActivityFailed, "problem")
	if err := s.PruneActivityForTest(ctx); err != nil {
		t.Fatal(err)
	}
	sum, _ := s.ActivitySummary(ctx, []string{"ag_a"}, 30, time.Now())
	if n := len(s.RawActivityPeopleForTest(ctx)); n != 1 {
		t.Errorf("the old person-day is gone, today's stays: %d", n)
	}
	if len(sum.Events) != 1 {
		t.Errorf("the 40-day-old event is gone: %+v", sum.Events)
	}
	if err := s.DeleteAgent(ctx, "ag_a"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.RawActivityPeopleForTest(ctx)); n != 0 {
		t.Errorf("the agent's numbers go with it: %d", n)
	}
}

func TestActivity_MessagesAreCountedByTheMinuteAndOnlyRecentOnesShow(t *testing.T) {
	s, ctx := activityRig(t)
	for i := 0; i < 3; i++ {
		s.MessageHandled(ctx, "ag_a")
	}
	s.MessageHandled(ctx, "ag_b")
	s.MessageHandled(ctx, "")                                                       // nobody: ignored
	s.MessageHandled(ctx, "nope")                                                   // an agent that doesn't exist: ignored
	if err := s.AddOldMessagesForTest(ctx, "ag_a", 30*time.Minute, 7); err != nil { // too long ago to count as lately
		t.Fatal(err)
	}

	now := time.Now()
	a, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if a.RecentMessages != 3 || a.RecentWindowMinutes != 10 {
		t.Errorf("3 in the last 10 minutes, the 7 from half an hour ago don't count: %+v", a)
	}
	both, _ := s.ActivitySummary(ctx, []string{"ag_a", "ag_b"}, 7, now)
	if both.RecentMessages != 4 {
		t.Errorf("all agents add up: %d", both.RecentMessages)
	}
	if later, _ := s.ActivitySummary(ctx, []string{"ag_a"}, 7, now.Add(12*time.Minute)); later.RecentMessages != 0 {
		t.Errorf("twelve minutes later they are no longer recent: %d", later.RecentMessages)
	}
	none, _ := s.ActivitySummary(ctx, nil, 7, now)
	if none.RecentMessages != 0 || none.RecentWindowMinutes != 10 {
		t.Errorf("no agents: %+v", none)
	}

	// A day old, they are gone; the agent going takes the rest with it.
	if err := s.AddOldMessagesForTest(ctx, "ag_a", 30*time.Hour, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneActivityForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.MinuteRowsForTest(ctx, "ag_a"); n != 2 { // this minute and half an hour ago; not 30 hours ago
		t.Errorf("rows after tidying: %d", n)
	}
	if err := s.DeleteAgent(ctx, "ag_a"); err != nil {
		t.Fatal(err)
	}
	if n := s.MinuteRowsForTest(ctx, "ag_a"); n != 0 {
		t.Errorf("the agent's counts go with it: %d", n)
	}
}

// What messages were handled with is counted per capability per day, summed over the period most first, with the day
// counting began; nothing else about the message is kept.
func TestActivity_UsesAreCountedPerCapabilityAndSummedOverThePeriod(t *testing.T) {
	s, ctx := activityRig(t)
	if sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now()); err != nil || sum.Uses == nil || len(sum.Uses) != 0 || sum.UsesSince != "" {
		t.Fatalf("nothing counted yet: %+v %v", sum, err)
	}
	for i := 0; i < 3; i++ {
		s.CapabilitiesUsed(ctx, "ag_a", []string{"sk_prs"})
	}
	s.CapabilitiesUsed(ctx, "ag_a", []string{"sk_prs", "sk_mail", "sk_prs"}) // one message, two capabilities: each once
	s.CapabilitiesUsed(ctx, "ag_a", nil)                                     // just talking
	s.CapabilitiesUsed(ctx, "ag_a", []string{})
	s.CapabilitiesUsed(ctx, "ag_b", []string{"sk_other"})
	s.CapabilitiesUsed(ctx, "", []string{"sk_prs"}) // no agent: ignored

	sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, u := range sum.Uses {
		got[u.Name] = u.Count
	}
	if len(sum.Uses) != 3 || sum.Uses[0].Name != "sk_prs" || got["sk_prs"] != 4 || got["sk_mail"] != 1 || got[""] != 2 {
		t.Errorf("per capability, most first, another agent's left out: %+v", sum.Uses)
	}
	if sum.UsesSince != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("counted since today: %q", sum.UsesSince)
	}

	// Older than the period: not in the sum, but counting still began then. Older than kept: removed.
	if err := s.AgeUsesForTest(ctx, "ag_a", 10); err != nil {
		t.Fatal(err)
	}
	s.CapabilitiesUsed(ctx, "ag_a", []string{"sk_mail"})
	sum, _ = s.ActivitySummary(ctx, []string{"ag_a"}, 7, time.Now())
	if len(sum.Uses) != 1 || sum.Uses[0].Name != "sk_mail" || sum.Uses[0].Count != 1 {
		t.Errorf("only the period: %+v", sum.Uses)
	}
	if want := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02"); sum.UsesSince != want {
		t.Errorf("counting began ten days ago: %q, want %q", sum.UsesSince, want)
	}
	if err := s.AgeUsesForTest(ctx, "ag_a", 400); err != nil {
		t.Fatal(err)
	}
	_ = s.PruneActivityForTest(ctx)
	if sum, _ := s.ActivitySummary(ctx, []string{"ag_a"}, 30, time.Now()); sum.UsesSince != "" {
		t.Errorf("old counts are removed like the rest of the activity: %q", sum.UsesSince)
	}
}

// Someone who came back is someone active on more than one of the last seven days, counted once.
func TestActivity_PeopleWhoCameBackAreThoseActiveOnMoreThanOneDayThisWeek(t *testing.T) {
	s, ctx := activityRig(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s.SetActivityDaysForTest(ctx, "ag_a", map[string][]string{
		"2026-10-19": {"p1", "p2", "p4"},
		"2026-10-18": {"p1"},
		"2026-10-15": {"p1", "p2"},
		"2026-10-13": {"p3"}, // the first of the seven days
		"2026-10-12": {"p4"}, // outside the week: p4 came once this week
	})
	sum, err := s.ActivitySummary(ctx, []string{"ag_a"}, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ActiveWeek != 4 || sum.ReturningWeek != 2 {
		t.Errorf("4 people this week, 2 of them (p1, p2) on more than one day: %+v", sum)
	}
}
