package domain

import (
	"context"
	"fmt"
	"strings"
)

// What an agent may claim, and how that is checked.
//
// An agent ends a turn by calling "finish" with some text, and nothing used to tie
// that text to what the agent actually did: it could say a trade was placed having
// never touched the trade tool. These rules tie them together without reading the
// wording.
//
//  1. Every action the loop runs is recorded, with whether it REALLY did its job
//     (OutcomeOf). Only tools that change something (ActionSpec.Changes) can count.
//  2. For a request to DO something, the host says which action(s) must have run for
//     the request to be done (WithRequirements, decided by Jev reading the request).
//  3. Finishing then carries an explicit outcome: "done", "need_details" or "cannot".
//     "done" is accepted only if every required action really succeeded this turn;
//     otherwise the agent is sent back with the facts. The two honest endings are
//     always allowed.

// Outputs the gates in front of a tool return when the tool did not run.
const (
	// OutputNotApproved is what a tool returns when the person declined (or let the
	// approval lapse).
	OutputNotApproved = "not approved"
	// OutputAuthPendingPrefix starts what a tool returns when the person has to
	// connect an account first and a prompt was sent to them.
	OutputAuthPendingPrefix = "auth_pending"
)

// How a turn can end.
const (
	OutcomeDone        = "done"
	OutcomeNeedDetails = "need_details"
	OutcomeCannot      = "cannot"
)

// maxFinishBounces is how many times a false "done" is sent back before the loop
// stops arguing and states the facts itself.
const maxFinishBounces = 2

// ActionRecord is what happened when the loop ran one action.
type ActionRecord struct {
	Name    string
	Changes bool   // it is a tool that changes something
	Done    bool   // it really did its job
	Message string // the tool's own plain-words message, when it gave one
	Reason  string // when not done: why, in plain words
}

// OutcomeOf decides, from what an action returned, whether it really did its job.
//
//	an error                                  → not done: it didn't go through
//	"not approved"                            → not done: it wasn't approved
//	"auth_pending…"                           → not done: the person must connect first
//	an object with "changed": false           → not done: its own message says why
//	anything else                             → done (an object's "message" is kept)
//
// A tool that can fail without raising an error should say so by returning
// "changed": false.
func OutcomeOf(output any, err error) (done bool, message, reason string) {
	if err != nil {
		return false, "", "it didn't go through"
	}
	switch v := output.(type) {
	case string:
		switch {
		case v == OutputNotApproved:
			return false, "", "it wasn't approved"
		case strings.HasPrefix(v, OutputAuthPendingPrefix):
			return false, "", "you need to connect your account first"
		}
	case map[string]any:
		message, _ = v["message"].(string)
		if changed, ok := v["changed"].(bool); ok && !changed {
			reason = strings.TrimSpace(message)
			if reason == "" {
				reason = "nothing was changed"
			}
			return false, message, strings.TrimRight(reason, ".")
		}
	}
	return true, message, ""
}

// Requirements say what must have really happened for a turn to be called done.
type Requirements struct {
	// Tools are the names of the actions that must each have succeeded.
	Tools []string
	// AnyChange, when set and Tools is empty, is satisfied by any one action that
	// changes something having succeeded. It is the fallback when it can't be
	// worked out which action the request needs.
	AnyChange bool
}

// Active reports whether anything is required.
func (r *Requirements) Active() bool {
	return r != nil && (len(r.Tools) > 0 || r.AnyChange)
}

type requirementsKey struct{}

// WithRequirements makes a loop run under the given requirements.
func WithRequirements(ctx context.Context, r *Requirements) context.Context {
	return context.WithValue(ctx, requirementsKey{}, r)
}

// RequirementsFromContext returns the requirements for this run, or nil.
func RequirementsFromContext(ctx context.Context) *Requirements {
	r, _ := ctx.Value(requirementsKey{}).(*Requirements)
	return r
}

// satisfied reports whether the records meet the requirements.
func (r *Requirements) satisfied(records []ActionRecord) bool {
	if len(r.Tools) == 0 {
		for _, rec := range records {
			if rec.Changes && rec.Done {
				return true
			}
		}
		return false
	}
	for _, name := range r.Tools {
		ok := false
		for _, rec := range records {
			if rec.Name == name && rec.Done {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// lastAttempt finds the most recent attempt at a required action (or at any
// changing action when none is named), so the reply can say why it didn't happen.
func (r *Requirements) lastAttempt(records []ActionRecord) *ActionRecord {
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if len(r.Tools) == 0 {
			if rec.Changes {
				return &rec
			}
			continue
		}
		for _, name := range r.Tools {
			if rec.Name == name {
				return &rec
			}
		}
	}
	return nil
}

// NothingChangedMessage states plainly that the request was not carried out, and
// why, from what actually happened. Used when the agent keeps claiming otherwise.
func (r *Requirements) NothingChangedMessage(records []ActionRecord) string {
	reason := "I didn't carry that out"
	if att := r.lastAttempt(records); att != nil && !att.Done && att.Reason != "" {
		reason = att.Reason
	}
	return "Nothing was changed: " + reason + "."
}

// finishCheck judges a finish call against the requirements and the record. It
// returns ok, or feedback to send back to the agent.
func finishCheck(args map[string]any, r *Requirements, records []ActionRecord) (ok bool, feedback string) {
	outcome, _ := args["outcome"].(string)
	switch outcome {
	case OutcomeNeedDetails, OutcomeCannot:
		return true, ""
	case OutcomeDone:
		if r.satisfied(records) {
			return true, ""
		}
		what := "the action you were asked to carry out"
		if len(r.Tools) > 0 {
			what = fmt.Sprintf("%q", strings.Join(r.Tools, `", "`))
		}
		msg := fmt.Sprintf("You finished with outcome %q, but %s has not succeeded this turn.", OutcomeDone, what)
		if att := r.lastAttempt(records); att != nil && !att.Done {
			msg += fmt.Sprintf(" The last attempt did not go through: %s.", att.Reason)
		} else if r.lastAttempt(records) == nil {
			msg += " You have not tried it."
		}
		msg += fmt.Sprintf(" Call the action now. If you can't (for example you need a detail from the person, or their account isn't connected), finish with outcome %q or %q and say so. Do not tell the person it is done unless it is.", OutcomeNeedDetails, OutcomeCannot)
		return false, msg
	default:
		return false, fmt.Sprintf("Say how this ended: set outcome to %q, %q or %q.", OutcomeDone, OutcomeNeedDetails, OutcomeCannot)
	}
}

// receipts are the tools' own messages for what really was done.
func receipts(records []ActionRecord) []string {
	var out []string
	seen := map[string]bool{}
	for _, rec := range records {
		if rec.Changes && rec.Done && strings.TrimSpace(rec.Message) != "" && !seen[rec.Message] {
			seen[rec.Message] = true
			out = append(out, "✓ "+strings.TrimSpace(rec.Message))
		}
	}
	return out
}
