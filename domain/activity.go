package domain

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Activity is how an agent is doing, for the creator who owns it: how many people are connected
// and active, what went wrong, and what people asked for that it couldn't do. It is counts and
// plain categories only. It never holds what anyone wrote, never who they are, and never their
// own tasks: a person is only ever a code that lets the same person be counted once.

// ActivityKind is what kind of thing happened.
type ActivityKind string

const (
	ActivityJoined      ActivityKind = "joined"      // someone connected to a chat app (detail: the app)
	ActivityFailed      ActivityKind = "failed"      // a reply didn't go through (detail: "unavailable" or "problem")
	ActivityUnsupported ActivityKind = "unsupported" // someone asked for something it can't do (detail: the topic)
	ActivitySignIn      ActivityKind = "signin"      // someone was asked to connect or sign in to a service (detail: the service)
)

// ActivityRecorder is what the host tells when something happens. Both calls are best-effort: they
// never block or fail a reply.
type ActivityRecorder interface {
	// PersonActive notes that a person sent a message to an agent. person is any stable identifier
	// of them; only an irreversible code of it is kept, to count each person once.
	PersonActive(ctx context.Context, agentID, person string)
	// ActivityEvent notes that something happened. detail is cleaned (see CleanActivityDetail).
	ActivityEvent(ctx context.Context, agentID string, kind ActivityKind, detail string)
}

// ActivityEvent is one thing that happened, with no identity in it.
type ActivityEvent struct {
	ID      int64        `json:"id"`
	AgentID string       `json:"agent_id"`
	Kind    ActivityKind `json:"kind"`
	Detail  string       `json:"detail"`
	At      time.Time    `json:"at"`
}

// ActivityDay is how many people were active on one day (UTC), as YYYY-MM-DD.
type ActivityDay struct {
	Day    string `json:"day"`
	People int    `json:"people"`
}

// ActivityCount is a name with how many.
type ActivityCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ActivityProblem is something that went wrong lately (the last day), grouped.
type ActivityProblem struct {
	AgentID string       `json:"agent_id"`
	Kind    ActivityKind `json:"kind"`
	Detail  string       `json:"detail"`
	Count   int          `json:"count"`
	Last    time.Time    `json:"last"`
}

// ActivitySummary is everything the creator's Activity screen shows.
type ActivitySummary struct {
	Connected         int             `json:"connected"`
	ConnectedThisWeek int             `json:"connected_this_week"`
	ActiveToday       int             `json:"active_today"`
	ActiveWeek        int             `json:"active_week"`
	FailedToday       int             `json:"failed_today"`
	Daily             []ActivityDay   `json:"daily"`
	ChatApps          []ActivityCount `json:"chat_apps"`
	// Topics are what people asked for that the agent couldn't do. A topic only appears once it has
	// been asked for a few times, so it can never point at one person.
	Topics   []ActivityCount   `json:"topics"`
	Problems []ActivityProblem `json:"problems"`
	Events   []ActivityEvent   `json:"events"`
}

// MinTopicCount is how often something must have been asked for before it is shown as a topic.
const MinTopicCount = 3

// ActivityReader reads the numbers for some agents over a number of days (1 to 30).
type ActivityReader interface {
	ActivitySummary(ctx context.Context, agentIDs []string, days int, now time.Time) (ActivitySummary, error)
}

const maxActivityDetail = 40

// CleanActivityDetail makes a detail safe to keep and to show: lower-case words only (letters,
// spaces, hyphens, apostrophes, dots), no digits, no addresses, at most 40 characters. A detail
// that has nothing left is "". Used on everything recorded, so a topic can't carry a number, an
// email or a name-and-number by accident.
func CleanActivityDetail(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || r == '-' || r == '\'' || r == '’' || r == '.':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case unicode.IsSpace(r), unicode.IsDigit(r), r == '@', r == '_', r == '/', r == ':':
			space = true
		}
		if utf8.RuneCountInString(b.String()) >= maxActivityDetail {
			break
		}
	}
	out := strings.Trim(b.String(), " .-'’")
	if r := []rune(out); len(r) > maxActivityDetail {
		out = strings.TrimSpace(string(r[:maxActivityDetail]))
	}
	if utf8.RuneCountInString(out) < 3 {
		return ""
	}
	return out
}
