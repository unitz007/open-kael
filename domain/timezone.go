package domain

import (
	"context"
	"strings"
)

// A person's time zone is needed whenever something depends on their own clock. When it is not known
// yet, a turn that reaches such a thing asks for it once, with one tap, and then carries on with what
// the person asked: nothing quietly assumes UTC.

// TimezoneRequester asks a person for their time zone. It sends the one-tap prompt, keeps what they
// asked so it is carried on once they answer, and returns at once.
type TimezoneRequester interface {
	RequestTimezone(ctx context.Context, conv ConversationRef, userID string) error
}

type ctxTimezoneRequester struct{}

// WithTimezoneRequester attaches a requester to ctx for a turn whose person has no time zone yet.
func WithTimezoneRequester(ctx context.Context, r TimezoneRequester) context.Context {
	return context.WithValue(ctx, ctxTimezoneRequester{}, r)
}

// TimezoneRequesterFromContext returns the requester for this turn, if there is one: there is none
// when the zone is already known, when nobody is there to ask (a repeating task), or when the
// messenger can't show the prompt.
func TimezoneRequesterFromContext(ctx context.Context) (TimezoneRequester, bool) {
	r, ok := ctx.Value(ctxTimezoneRequester{}).(TimezoneRequester)
	return r, ok
}

// AskForTimezone asks the person for their time zone if it is missing and it can be asked. It reports
// true when the prompt has been sent: whatever was about to happen waits, and is carried on after they answer.
func AskForTimezone(ctx context.Context) bool {
	if UserDetailsFromContext(ctx).Timezone != "" {
		return false
	}
	r, ok := TimezoneRequesterFromContext(ctx)
	if !ok {
		return false
	}
	conv, ok := ConversationFromContext(ctx)
	if !ok {
		return false
	}
	userID := conv.UserID
	if userID == "" {
		userID = UserIDFromContext(ctx)
	}
	if userID == "" {
		return false // nobody to ask
	}
	return r.RequestTimezone(ctx, conv, userID) == nil
}

// TimezoneGateIdentity is the key a request held for a time zone is kept under (it is not an account).
const TimezoneGateIdentity = "timezone"

// OutputTimezonePendingPrefix starts what a tool returns when the person has been asked for their
// time zone and what they asked will be carried on afterwards.
const OutputTimezonePendingPrefix = "timezone_pending"

const timezonePendingNote = OutputTimezonePendingPrefix + `: the person has been asked for their time zone, directly — do NOT write anything additional to them. Call finish now with content set to exactly empty string "".`

// withTimezoneGate makes a tool that depends on the person's clock ask for their time zone first, once.
func withTimezoneGate(def *ToolDefinition, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	if !def.NeedsLocalTime {
		return inner
	}
	return func(ctx context.Context, input map[string]any) (any, error) {
		if AskForTimezone(ctx) {
			return timezonePendingNote, nil
		}
		return inner(ctx, input)
	}
}

func isTimezonePending(v string) bool { return strings.HasPrefix(v, OutputTimezonePendingPrefix) }
