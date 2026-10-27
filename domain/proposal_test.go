package domain

import (
	"strings"
	"testing"
)

func TestProposalData_RoundTrips(t *testing.T) {
	for _, c := range []struct {
		id string
		i  int
	}{{"abc123", 0}, {"0123456789abcdef0123456789abcdef", 4}, {"a:b", 2}} {
		data := ProposalData(c.id, c.i)
		id, i, ok := ParseProposalData(data)
		if !ok || id != c.id || i != c.i {
			t.Errorf("%q → %q %d %v", data, id, i, ok)
		}
	}
	if d := ProposalData("0123456789abcdef0123456789abcdef", 4); len(d) > 64 {
		t.Errorf("a Telegram button's data may be at most 64 bytes: %d", len(d))
	}
	for _, bad := range []string{"", "prop:", "prop:abc", "prop::1", "prop:abc:", "prop:abc:x", "prop:abc:-1", "prop:abc:01", "choice-1:2", "kael_sm:link_account", "prop:abc:1 "} {
		if _, _, ok := ParseProposalData(bad); ok {
			t.Errorf("%q is not a proposal button", bad)
		}
	}
}

func choice(label, say string) map[string]any { return map[string]any{"label": label, "say": say} }

// A button whose words say when, and whose label doesn't, gets the time in its label: "Check USD/CAD order" for
// "Check back on my USD/CAD limit order later" was tapped as "check it now" (2026-10-27). A label that says when in its
// own way (a clock time, a day) is left as it is, and one with no room for it too.
func TestParseProposalChoices_ALabelSaysWhen(t *testing.T) {
	got := ParseProposalChoices([]any{
		choice("Check USD/CAD order", "Check back on my USD/CAD limit order later."),
		choice("Check EUR/CHF trade", "Check back on my EUR/CHF sell trade this evening."),
		choice("Remind me", "Remind me tomorrow at 9 to call mum"),
		choice("Move dentist to 10:15", "Move my dentist appointment to 10:15 tomorrow"),
		choice("Check it now", "Check my USD/CAD order now"),
		choice("Summarise my whole inbox every week", "Summarise my whole inbox every Friday"),
	})
	for _, c := range []struct{ label, say string }{
		{"Weekly on Fridays", "Make this a weekly summary every Friday."},
		{"Morning rain timing", "When exactly will it stop raining tomorrow morning?"},
	} {
		if l := withTiming(c.label, c.say); l != c.label {
			t.Errorf("a label that says when in its own way is left as it is: %q → %q", c.label, l)
		}
	}
	want := []string{"Check USD/CAD order later", "Check EUR/CHF trade this evening", "Remind me tomorrow at 9", "Move dentist to 10:15", "Check it now", "Summarise my whole inbox every week"}
	if len(got) != MaxProposalChoices {
		t.Fatalf("%+v", got)
	}
	for i, w := range want[:MaxProposalChoices] {
		if got[i].Label != w {
			t.Errorf("%q → %q, want %q", got[i].Say, got[i].Label, w)
		}
	}
}

func TestParseProposalChoices(t *testing.T) {
	good := []any{choice("Move dentist to 10:15", "Move my dentist appointment to 10:15 tomorrow"), choice("Keep both", "Keep both appointments as they are")}
	got := ParseProposalChoices(good)
	if len(got) != 2 || got[0].Label != "Move dentist to 10:15" || got[1].Say != "Keep both appointments as they are" {
		t.Fatalf("%+v", got)
	}

	for name, raw := range map[string]any{
		"nothing":                   nil,
		"not a list":                "buttons please",
		"one is no choice":          []any{choice("Only", "Only one")},
		"one good, one empty label": []any{choice("Fine", "Fine say"), choice("", "no label")},
		"one good, one empty say":   []any{choice("Fine", "Fine say"), choice("No say", "  ")},
		"same label twice":          []any{choice("Same", "one"), choice("same", "two")},
		"label too long":            []any{choice("Fine", "x"), choice(strings.Repeat("a", MaxChoiceLabelRunes+1), "x")},
		"say too long":              []any{choice("Fine", "x"), choice("Long", strings.Repeat("a", MaxChoiceSayRunes+1))},
		"wrong shape":               []any{"a", "b"},
	} {
		if got := ParseProposalChoices(raw); got != nil {
			t.Errorf("%s: a half-formed offer must give no buttons at all, got %+v", name, got)
		}
	}

	// Bad ones are dropped when enough good ones remain; whitespace is tidied; the number is capped.
	mixed := []any{choice("  Spread   out ", "  say 1 "), choice("", "x"), choice("B", "say 2"), "junk", choice("C", "say 3")}
	got = ParseProposalChoices(mixed)
	if len(got) != 3 || got[0].Label != "Spread out" || got[0].Say != "say 1" {
		t.Errorf("%+v", got)
	}
	var many []any
	for i := 0; i < 9; i++ {
		many = append(many, choice(string(rune('A'+i)), "say"))
	}
	if got := ParseProposalChoices(many); len(got) != MaxProposalChoices {
		t.Errorf("at most %d buttons: %d", MaxProposalChoices, len(got))
	}
	// A typed list works too (what code, rather than JSON, would build).
	if got := ParseProposalChoices([]map[string]any{choice("A", "a"), choice("B", "b")}); len(got) != 2 {
		t.Errorf("%+v", got)
	}
}
