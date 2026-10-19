package domain

import "testing"

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ada@example.com":      "ada@example.com",
		"Ada@Example.COM":      "ada@example.com",
		"  ada@example.com \n": "ada@example.com",
		"ADA+Tag@Example.com":  "ada+tag@example.com",
		"":                     "",
		"   ":                  "",
		"É@Example.com":        "é@example.com",
	} {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if NormalizeEmail(NormalizeEmail(" A@B.co ")) != "a@b.co" {
		t.Error("normalising twice changes nothing")
	}
}
