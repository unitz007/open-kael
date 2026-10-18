package domain

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPersonNote_SaysWhoAndWhenInThePersonsOwnTime(t *testing.T) {
	now := time.Date(2026, 10, 17, 19, 6, 40, 0, time.UTC)
	got := PersonNote(UserDetails{Name: "Ada", Language: "fr", Country: "NG", City: "Lagos", Timezone: "Africa/Lagos"}, now)
	for _, want := range []string{
		`called "Ada"`, "live in Lagos, Nigeria", "preferred language is French",
		"Saturday, 17 October 2026, 20:06 (Africa/Lagos, the person's time zone); that is 19:06 UTC",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// Nobody known: still says when it is, in UTC, and does not guess a time zone.
	got = PersonNote(UserDetails{}, now)
	if !strings.Contains(got, "Saturday, 17 October 2026, 19:06 UTC") || strings.Contains(got, "called") {
		t.Errorf("no details: %q", got)
	}
}

type captureLoop struct{ system string }

func (c *captureLoop) Run(_ context.Context, messages []Message, _ []*BoundAction) (*LoopResult, []Message, error) {
	for _, m := range messages {
		if m.Role == RoleSystem {
			c.system = m.Content
		}
	}
	return &LoopResult{Status: LoopStatusComplete}, messages, nil
}

// An ability that runs its own loop (several tools) is told when it is and who it works for,
// however it was started: a chat turn, a schedule, an event.
func TestBindSkill_AnAbilitysOwnLoopKnowsWhenItIsAndWhoItWorksFor(t *testing.T) {
	old := skillClock
	skillClock = func() time.Time { return time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC) }
	defer func() { skillClock = old }()

	run := func(ctx context.Context) string {
		loop := &captureLoop{}
		skill := &Skill{ID: "s", Name: "morning_summary", Instructions: "Summarise my day."}
		tools := []*BoundAction{{Spec: ActionSpec{Name: "a"}}, {Spec: ActionSpec{Name: "b"}}}
		action := BindSkill(&Agent{ID: "a1", Loop: loop}, skill, tools)
		if _, err := action.Invoke(ctx, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		return loop.system
	}

	// A scheduled run for a known person: their name and their morning.
	got := run(WithUserDetails(context.Background(), UserDetails{Name: "Ada", Timezone: "Africa/Lagos"}))
	if !strings.HasPrefix(got, "Summarise my day.") || !strings.Contains(got, `called "Ada"`) || !strings.Contains(got, "Sunday, 18 October 2026, 08:00 (Africa/Lagos") {
		t.Errorf("the ability's instructions must carry the person and their local time: %q", got)
	}
	// A run with no person (a creator's cron, an event): still knows when it is.
	got = run(context.Background())
	if !strings.Contains(got, "Sunday, 18 October 2026, 07:00 UTC") || strings.Contains(got, "called") {
		t.Errorf("a headless run still knows the date: %q", got)
	}
}

func TestSkillLabel_IsPlainWordsNeverTheInternalName(t *testing.T) {
	cases := []struct {
		skill *Skill
		want  string
	}{
		{&Skill{Name: "manages_fpl_squad_transfers", Description: "Manages my FPL squad and transfers"}, "Manages my FPL squad and transfers"},
		{&Skill{Name: "s_calendar_finds_free", Description: "  Checks your calendar  "}, "Checks your calendar"},
		{&Skill{Name: "places_forex_trades"}, "places forex trades"},
		{&Skill{}, "your task"},
		{nil, "your task"},
	}
	for _, c := range cases {
		got := SkillLabel(c.skill)
		if got != c.want || strings.Contains(got, "_") {
			t.Errorf("label %q, want %q", got, c.want)
		}
	}
}
