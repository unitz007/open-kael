package domain

import (
	"strings"
	"testing"
)

func TestCleanActivityDetail_KeepsOnlyPlainWords(t *testing.T) {
	for in, want := range map[string]string{
		"Book a flight":                      "book a flight",
		"  Convert   a currency!! ":          "convert a currency",
		"pay Ada's bill 12345":               "pay ada's bill",
		"email me at ada@example.com please": "email me at ada example.com please",
		"my card 4111 1111 1111 1111":        "my card",
		"2026-10-19":                         "",
		"ok":                                 "",
		"":                                   "",
		"voice-message":                      "voice-message",
		"Überweisung machen":                 "überweisung machen",
		strings.Repeat("word ", 30):          strings.TrimSpace(strings.Repeat("word ", 8)),
	} {
		if got := CleanActivityDetail(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// Nothing digit-shaped or address-shaped can survive, whatever it is wrapped in.
func TestCleanActivityDetail_NeverKeepsDigitsOrAtSigns(t *testing.T) {
	for _, in := range []string{"call 07911 123456", "a@b.co", "code 998877 now", "x1y2z3 tracking"} {
		if got := CleanActivityDetail(in); strings.ContainsAny(got, "0123456789@") {
			t.Errorf("%q → %q", in, got)
		}
	}
}
