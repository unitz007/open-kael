package domain

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// What an agent has learned about a person, as a map of named details: name, country, residence, and
// so on. There are two kinds of map for each person: a personal one (who they are) and one for each
// connected app (how they use it). A detail is learned one at a time. A new one is saved straight
// away; one we already hold is only ever replaced after the person says yes.

// ProfilePersonal is the scope of the personal map. The map for an app has the app's integration ID as its scope.
const ProfilePersonal = "personal"

// Where a detail came from.
const (
	DetailSaid    = "said"    // the person said it in conversation
	DetailAsked   = "asked"   // they answered a question we asked
	DetailDevice  = "device"  // their own device reported it (time zone)
	DetailChanged = "changed" // they changed it themselves, after we proposed it or by editing
	DetailMoved   = "moved"   // carried over from the older free-text note
)

const (
	// MaxDetailsPerScope bounds one map, so nothing can flood it.
	MaxDetailsPerScope = 60
	// MaxDetailKeyLen and MaxDetailValueRunes bound one detail.
	MaxDetailKeyLen     = 40
	MaxDetailValueRunes = 200
)

// ProfileDetail is one named detail of a person, in a scope.
type ProfileDetail struct {
	Scope       string
	Key         string
	Value       string
	Source      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ConfirmedAt time.Time // when it was last known to be right
}

// ErrProfileFull means a map already holds as many details as it may.
var ErrProfileFull = errors.New("profile: too many details")

// ProfileDetailStore keeps the maps. Postgres implements it.
type ProfileDetailStore interface {
	// ProfileDetails returns a person's details in the given scopes (every scope when none is given).
	ProfileDetails(ctx context.Context, userID string, scopes ...string) ([]ProfileDetail, error)
	// SetProfileDetail saves a detail, replacing any value held under the same key. The caller has
	// already decided that is allowed (a replacement needs the person's yes).
	SetProfileDetail(ctx context.Context, userID string, d ProfileDetail) error
	// ConfirmProfileDetail notes that a detail was just confirmed to still be right.
	ConfirmProfileDetail(ctx context.Context, userID, scope, key string) error
	// RemoveProfileDetail deletes one detail; it reports whether it was there.
	RemoveProfileDetail(ctx context.Context, userID, scope, key string) (bool, error)
	// DeclineProfileValue remembers that a value was proposed for a detail and refused.
	DeclineProfileValue(ctx context.Context, userID, scope, key, value string) error
	// DeclinedProfileValue reports whether this value (compared loosely) was refused since the given time.
	DeclinedProfileValue(ctx context.Context, userID, scope, key, value string, since time.Time) (bool, error)
	// ProfileMigrated and MarkProfileMigrated record that a person's older free-text note for a
	// scope has been moved into details.
	ProfileMigrated(ctx context.Context, userID, scope string) (bool, error)
	MarkProfileMigrated(ctx context.Context, userID, scope string) error
}

// ── Names ────────────────────────────────────────────────────────────────────

var nonKeyChars = regexp.MustCompile(`[^a-z0-9]+`)

// NormalizeDetailKey makes a detail's name consistent: lower case, words joined with underscores, no
// odd characters, American spellings of "favorite" folded to ours. "Country", "country " and
// "COUNTRY" are one detail. It reports false when nothing usable is left.
func NormalizeDetailKey(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "favorite", "favourite")
	s = nonKeyChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if r := []rune(s); len(r) > MaxDetailKeyLen {
		s = strings.Trim(string(r[:MaxDetailKeyLen]), "_")
	}
	return s, len(s) >= 2
}

// detailAliases maps the usual other names for a detail onto one standard name, so the same fact said
// another way is one detail, not two. Anything not listed here is left for the similarity check.
var detailAliases = map[string]string{
	"location": "residence", "city": "residence", "lives_in": "residence", "based_in": "residence", "home_city": "residence",
	"city_of_residence": "residence", "place_of_residence": "residence", "home_location": "residence", "current_city": "residence",
	"full_name": "name", "legal_name": "name", "fullname": "name",
	"preferred_language": "language", "main_language": "language",
	"timezone": "time_zone", "tz": "time_zone", "time_zone_name": "time_zone",
	"job": "occupation", "profession": "occupation", "job_title": "occupation", "role": "occupation", "current_job": "occupation",
	"company": "employer", "works_at": "employer", "workplace": "employer", "organization": "employer", "organisation": "employer", "employer_name": "employer",
	"dob": "birthday", "date_of_birth": "birthday", "birth_date": "birthday", "birthdate": "birthday",
	"favourite_team": "favourite_club", "football_team": "favourite_club", "team": "favourite_club", "supports": "favourite_club",
	"interests": "hobbies", "hobby": "hobbies", "pastimes": "hobbies",
	"home_country": "country", "country_of_origin": "country",
}

// CanonicalDetailKey tidies a name and folds a known synonym onto its standard name.
func CanonicalDetailKey(s string) (string, bool) {
	k, ok := NormalizeDetailKey(s)
	if !ok {
		return "", false
	}
	if a, ok := detailAliases[k]; ok {
		return a, true
	}
	return k, true
}

// ── Values ───────────────────────────────────────────────────────────────────

// CleanDetailValue tidies a value: one line, no control characters, extra spaces gone, at most
// MaxDetailValueRunes long. It reports false when nothing is left.
func CleanDetailValue(s string) (string, bool) {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
		case unicode.IsControl(r):
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > MaxDetailValueRunes {
		out = strings.TrimSpace(string([]rune(out)[:MaxDetailValueRunes]))
	}
	return out, out != ""
}

var stripMarks = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// ComparableValue reduces a value to what matters for deciding whether two values are the same:
// case, accents, punctuation and spacing do not count.
func ComparableValue(s string) string {
	t, _, err := transform.String(stripMarks, s)
	if err != nil {
		t = s
	}
	var b strings.Builder
	for _, r := range strings.ToLower(t) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SameDetailValue reports whether two values are the same once case, accents, punctuation and spacing
// are set aside: "Wolfsburg, Germany" and "wolfsburg germany" are the same.
func SameDetailValue(a, b string) bool {
	x, y := ComparableValue(a), ComparableValue(b)
	return x != "" && x == y
}

// ── What is never kept ───────────────────────────────────────────────────────

var sensitiveKey = regexp.MustCompile(`(^|_)(password|passcode|passphrase|pin|secret|token|api_key|apikey|private_key|cvv|cvc|card|iban|ssn|passport|licen[cs]e_number|bank_account|account_number|sort_code|routing)(_|$)`)
var longDigits = regexp.MustCompile(`\d[\d\s-]{8,}\d`)

// LooksSensitive reports whether a detail looks like something that must never be stored: a secret,
// a card or account number, an identity document. A learning step must drop these without asking.
func LooksSensitive(key, value string) bool {
	if sensitiveKey.MatchString(key) {
		return true
	}
	return longDigits.MatchString(value)
}

// ── Showing a map ────────────────────────────────────────────────────────────

// RenderDetails turns a map into the lines an agent is given, "key: value", in a steady order.
func RenderDetails(details []ProfileDetail) string {
	items := make([]ProfileDetail, len(details))
	copy(items, details)
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	var b strings.Builder
	for _, d := range items {
		if d.Key == "" || d.Value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(d.Key)
		b.WriteString(": ")
		b.WriteString(strings.ReplaceAll(d.Value, "\n", " "))
	}
	return b.String()
}
