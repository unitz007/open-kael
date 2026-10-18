package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

type userDetailsKey struct{}

// WithUserDetails attaches what a person has told us about themselves to ctx, so work
// done on their behalf (including an ability's own loop) can address them properly.
func WithUserDetails(ctx context.Context, d UserDetails) context.Context {
	return context.WithValue(ctx, userDetailsKey{}, d)
}

// UserDetailsFromContext returns what WithUserDetails attached (zero value if nothing).
func UserDetailsFromContext(ctx context.Context) UserDetails {
	d, _ := ctx.Value(userDetailsKey{}).(UserDetails)
	return d
}

// PersonContext turns what a person has told us into the lines an agent is given about
// them, and their time zone (nil when unknown or not a real zone). Anything unset or
// unrecognised is simply left out: the agent is never told something we can't stand behind.
// What the person typed is quoted, so it cannot pose as an instruction.
func PersonContext(d UserDetails) (lines string, loc *time.Location) {
	var b strings.Builder
	if name := strings.TrimSpace(d.Name); name != "" {
		fmt.Fprintf(&b, "\n\nThe person you are talking to is called %q. Use their name when it is natural.", name)
	}
	var place []string
	if city := strings.TrimSpace(d.City); city != "" {
		place = append(place, city)
	}
	if r, err := language.ParseRegion(d.Country); err == nil && r.IsCountry() {
		if name := display.English.Regions().Name(r); name != "" {
			place = append(place, name)
		}
	}
	if len(place) > 0 {
		fmt.Fprintf(&b, "\n\nThey live in %s. Keep local details (currency, units, holidays, opening hours) in line with that.", strings.Join(place, ", "))
	}
	if d.Language != "" {
		if tag, err := language.Parse(d.Language); err == nil {
			fmt.Fprintf(&b, "\n\nTheir preferred language is %s. Reply in it, unless they write to you in a different language.", display.English.Tags().Name(tag))
		}
	}
	if d.Timezone != "" {
		if l, err := time.LoadLocation(d.Timezone); err == nil {
			loc = l
		}
	}
	return b.String(), loc
}

// TimeNote tells an agent when it is, to the minute: in the person's own time zone when
// they have given one, otherwise in UTC.
func TimeNote(loc *time.Location, now time.Time) string {
	now = now.UTC()
	if loc != nil {
		local := now.In(loc)
		return fmt.Sprintf("\n\nRight now it is %s, %s (%s, the person's time zone); that is %s UTC. Use this for \"today\", \"tomorrow\", \"this week\" and anything about timing, and speak in their local time.",
			local.Weekday(), local.Format("2 January 2006, 15:04"), loc.String(), now.Format("15:04"))
	}
	return fmt.Sprintf("\n\nRight now it is %s, %s UTC. Use this for \"today\", \"tomorrow\", \"this week\" and anything about timing. You do not know the person's time zone unless they have told you; if their local time matters, ask rather than assume.",
		now.Weekday(), now.Format("2 January 2006, 15:04"))
}

// PersonNote is who the agent is working for and what time it is for them: the lines every
// agent loop, whether a chat turn or a scheduled run, is given.
func PersonNote(d UserDetails, now time.Time) string {
	lines, loc := PersonContext(d)
	return lines + TimeNote(loc, now)
}
