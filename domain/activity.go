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
	// MessageHandled counts one message from a person that the agent handled. Only the count, by
	// the minute, is kept.
	MessageHandled(ctx context.Context, agentID string)
}

// UseRecorder counts what a person's message was handled with: which of the agent's capabilities (by id), or none
// (an empty list: talking, or answering from what it knows). Only the count per capability per day is kept. A recorder
// that is not one simply does not count uses.
type UseRecorder interface {
	CapabilitiesUsed(ctx context.Context, agentID string, skillIDs []string)
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
	// Agents are the agents it was counted for, where that is worth saying (what people asked for that an agent
	// couldn't do).
	Agents []string `json:"agents,omitempty"`
}

// ActivityAgent is one agent's own numbers, so a creator with several can compare them. Counts only.
type ActivityAgent struct {
	AgentID     string `json:"agent_id"`
	ActiveToday int    `json:"active_today"`
	ActiveWeek  int    `json:"active_week"`
	FailedToday int    `json:"failed_today"`
	// Daily is how many people were active each day, in the same order as ActivitySummary.Daily.
	Daily []int `json:"daily"`
	// TopUse is its most used capability over the period (Name is the capability id), when anything was counted.
	TopUse *ActivityCount `json:"top_use,omitempty"`
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
	// RecentMessages is how many messages the agents handled in the last RecentWindowMinutes:
	// shown as one line in the feed, not one line per message.
	RecentMessages      int `json:"recent_messages"`
	RecentWindowMinutes int `json:"recent_window_minutes"`
	// ReturningWeek is how many of the people active in the last seven days were active on more than one of them.
	ReturningWeek int `json:"returning_week"`
	// Uses is how many messages were handled with each capability over the period, by capability id ("" = with none),
	// most first. UsesSince is the first day anything was counted for these agents ("" when nothing has been), so a
	// screen can say the counting is newer than the period.
	Uses      []ActivityCount `json:"uses"`
	UsesSince string          `json:"uses_since"`
	// ByAgent is each agent's own numbers, in the order the agents were asked for.
	ByAgent []ActivityAgent `json:"by_agent"`
	// UsesDaily is Uses day by day for each capability (Name is the capability id; "with none" is left out), in the
	// same order as Daily, so a screen can draw what a connection was used for each day.
	UsesDaily []ActivitySeries `json:"uses_daily"`
	// SignedIn is, for each service (Name is the integration id), how many of the people who chat with these agents
	// have connected their own account to it; SignedInPeople is how many have connected at least one. Counts only.
	SignedIn       []ActivityCount `json:"signed_in"`
	SignedInPeople int             `json:"signed_in_people"`
}

// ActivitySeries is a name with a count for each day of the period, in the order of ActivitySummary.Daily.
type ActivitySeries struct {
	Name  string `json:"name"`
	Daily []int  `json:"daily"`
}

// RecentMessagesWindow is how far back "messages handled lately" looks.
const RecentMessagesWindow = 10 * time.Minute

// MinTopicCount is how often something must have been asked for before it is shown as a topic.
const MinTopicCount = 3

// ActivityReader reads the numbers for some agents over a number of days (1 to 30).
type ActivityReader interface {
	ActivitySummary(ctx context.Context, agentIDs []string, days int, now time.Time) (ActivitySummary, error)
}

// ActivityWatcher tells when something changed for some agents, so a screen can be pushed an update
// the moment it happens instead of asking over and over. It says only THAT something changed.
type ActivityWatcher interface {
	// WatchActivity returns a channel that receives (at most one pending signal, never blocking the
	// sender) when anything is recorded for any of these agents, and a function that stops watching.
	WatchActivity(agentIDs []string) (changed <-chan struct{}, stop func())
}

// WorkWatcher says which agents are in the middle of something right now (handling a message, or a person's alert or
// repeating task), so a screen can show the agent at work. It is about this moment only and keeps nothing.
type WorkWatcher interface {
	// Working returns those of these agents that are working now.
	Working(agentIDs []string) []string
	// WatchWork returns a channel that receives (at most one pending signal, never blocking the sender) when any of
	// these agents starts or stops working, and a function that stops watching.
	WatchWork(agentIDs []string) (changed <-chan struct{}, stop func())
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
