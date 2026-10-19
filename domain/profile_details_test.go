package domain

import (
	"strings"
	"testing"
)

func TestNormalizeAndCanonicalDetailKey(t *testing.T) {
	for in, want := range map[string]string{
		"Country":          "country",
		" country ":        "country",
		"COUNTRY":          "country",
		"Home Country":     "country",
		"lives-in":         "residence",
		"Based In":         "residence",
		"Location":         "residence",
		"City":             "residence",
		"Full Name":        "name",
		"TimeZone":         "time_zone",
		"timezone":         "time_zone",
		"favorite_food":    "favourite_food", // American spelling folded
		"Favorite Team":    "favourite_club",
		"Date of Birth":    "birthday",
		"Spouse Name":      "spouse_name",
		"Second Language!": "second_language",
	} {
		got, ok := CanonicalDetailKey(in)
		if !ok || got != want {
			t.Errorf("%q → %q (%v), want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "  ", "!", "a", "***"} {
		if got, ok := CanonicalDetailKey(bad); ok {
			t.Errorf("%q is not a usable name: %q", bad, got)
		}
	}
	long := strings.Repeat("word ", 40)
	if k, ok := CanonicalDetailKey(long); !ok || len([]rune(k)) > MaxDetailKeyLen || strings.HasSuffix(k, "_") {
		t.Errorf("a long name is cut tidily: %q", k)
	}
	// Look-alikes are different details and must stay different.
	for _, pair := range [][2]string{{"spouse_name", "name"}, {"second_language", "language"}, {"previous_residence", "residence"}, {"favourite_player", "favourite_club"}} {
		a, _ := CanonicalDetailKey(pair[0])
		b, _ := CanonicalDetailKey(pair[1])
		if a == b {
			t.Errorf("%q and %q must stay different details", pair[0], pair[1])
		}
	}
}

func TestCleanDetailValue(t *testing.T) {
	if v, ok := CleanDetailValue("  Wolfsburg,\n\t Germany \x00 "); !ok || v != "Wolfsburg, Germany" {
		t.Errorf("%q %v", v, ok)
	}
	if _, ok := CleanDetailValue(" \n\t "); ok {
		t.Error("nothing left is not a value")
	}
	if v, _ := CleanDetailValue(strings.Repeat("é", 500)); len([]rune(v)) != MaxDetailValueRunes {
		t.Errorf("cut to the limit by characters, not bytes: %d", len([]rune(v)))
	}
}

func TestSameDetailValue(t *testing.T) {
	for _, same := range [][2]string{
		{"Wolfsburg, Germany", "wolfsburg germany"}, {"Café", "cafe"}, {"Arsenal", "ARSENAL"}, {"12 March 1990", "12-march-1990"}, {"Europe/Berlin", "europe berlin"},
	} {
		if !SameDetailValue(same[0], same[1]) {
			t.Errorf("%q and %q are the same", same[0], same[1])
		}
	}
	for _, diff := range [][2]string{{"Wolfsburg", "Wolfsburg, Germany"}, {"Arsenal", "Chelsea"}, {"", ""}, {"!!!", "???"}} {
		if SameDetailValue(diff[0], diff[1]) {
			t.Errorf("%q and %q are not the same", diff[0], diff[1])
		}
	}
}

func TestLooksSensitive(t *testing.T) {
	for _, c := range []struct {
		k, v string
		want bool
	}{
		{"password", "hunter2", true}, {"bank_pin", "1234", true}, {"card_number", "x", true}, {"api_key", "x", true}, {"passport_number", "x", true},
		{"note", "my card is 4111 1111 1111 1111", true}, {"phone", "+49 5361 1234567", true}, {"iban_code", "x", true},
		{"residence", "Wolfsburg, Germany", false}, {"pin_code_area", "x", true}, {"pineapple", "likes it", false}, {"spinning", "weekly", false},
		{"birthday", "12 March 1990", false}, {"hobbies", "running, chess", false},
	} {
		if got := LooksSensitive(c.k, c.v); got != c.want {
			t.Errorf("%q=%q: %v, want %v", c.k, c.v, got, c.want)
		}
	}
}

func TestRenderDetails(t *testing.T) {
	got := RenderDetails([]ProfileDetail{{Key: "residence", Value: "Wolfsburg, Germany"}, {Key: "name", Value: "Charles\nDinneya"}, {Key: "empty", Value: ""}})
	if got != "name: Charles Dinneya\nresidence: Wolfsburg, Germany" {
		t.Errorf("%q", got)
	}
	if RenderDetails(nil) != "" {
		t.Error("nothing known renders nothing")
	}
}
