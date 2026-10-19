package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unitz007/open-kael/domain"
)

// A task whose time can't be written as a clock ("two hours before the deadline", "30 minutes before
// my first meeting") has no schedule in it. The agent works it out itself, and keeps working it out:
// each time it wakes it looks again with its own tools and sets when it wakes next. Nothing here
// knows what a deadline, a meeting or a flight is. What the code holds the agent to is only this:
// the time must come from something it looked up this time, must be in a sensible range, and must
// be said with the reason, so the person can always see why.

// WakeKind says what a wake-up is for.
type WakeKind string

const (
	// WakeAct is the time the task should be done. The agent is woken then and confirms, with
	// fresh facts, before it is done: a time that was right yesterday may have moved.
	WakeAct WakeKind = "act"
	// WakeCheck is a time to look again, because the time to act isn't known yet or is far off.
	WakeCheck WakeKind = "check"
)

const (
	// MinActAhead and MinCheckAhead are how soon the next wake-up may be. A look-up costs a model
	// call, so looking again is never more often than hourly.
	MinActAhead   = 15 * time.Minute
	MinCheckAhead = time.Hour
	// MaxWakeAhead is the furthest the next wake-up may be. Far-off times are reached by looking
	// again on the way, which also keeps a mistake from putting a task out of sight for long.
	MaxWakeAhead = 7 * 24 * time.Hour
	// LateWake is how late a wake-up may still do its task: after that the moment has passed.
	LateWake = time.Hour

	maxWakeReason    = 200
	maxWakeSay       = 300
	maxWakeIterating = 12
)

// WakeRecord is a wake-up that was planned earlier.
type WakeRecord struct {
	At     time.Time
	Kind   WakeKind
	Reason string
}

// WakeRequest asks the agent when a task should next happen.
type WakeRequest struct {
	AgentID, UserID string
	Action          string // what is done at the time: "Look at my squad and tell me who to drop"
	Timing          string // the person's words for when: "two hours before the FPL squad deadline"
	Zone            string // the person's time zone (IANA), "" for UTC
	Now             time.Time
	// Last is the wake-up planned before this one; nil for the first plan.
	Last *WakeRecord
	// Due is true when this is the wake-up itself, not the first plan.
	Due bool
}

// WakePlan is what the agent decided.
type WakePlan struct {
	At     time.Time
	Kind   WakeKind
	Reason string
	// DoItNow means the moment has come and still holds: do the task now. Only ever true for the
	// wake-up of an act time, on time, after the agent looked again.
	DoItNow bool
	// Say is set instead of a plan when the agent could not work it out: plain words for the person.
	Say string
}

// ErrNoWakePlan means the agent finished without setting a wake-up or saying why it couldn't.
var ErrNoWakePlan = errors.New("the agent did not set when to wake")

// PlanWake asks the agent to work out when a task should next happen, using what it can look up.
// It looks things up and changes nothing.
func (h *Host) PlanWake(ctx context.Context, req WakeRequest) (WakePlan, error) {
	h.mu.RLock()
	hosted, ok := h.agents[req.AgentID]
	h.mu.RUnlock()
	if !ok {
		return WakePlan{}, fmt.Errorf("PlanWake: agent %q not registered", req.AgentID)
	}
	if strings.TrimSpace(req.Action) == "" || strings.TrimSpace(req.Timing) == "" {
		return WakePlan{}, errors.New("PlanWake: a task and when it should happen are needed")
	}
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	loc := time.UTC
	if l, err := time.LoadLocation(req.Zone); err == nil && req.Zone != "" && req.Zone != "Local" {
		loc = l
	}

	// The same context a normal turn has (the person's connections and details), but nobody is
	// there, so nothing can wait for them.
	if req.UserID != "" && h.userConnectionRefLoader != nil {
		ctx = h.withUserConnectionRefs(ctx, req.UserID)
	}
	if req.UserID != "" && h.userToolApprovalLoader != nil {
		ctx = h.withUserToolApprovals(ctx, req.UserID)
	}
	ctx = domain.WithConversation(ctx, domain.ConversationRef{UserID: req.UserID, SenderID: req.UserID})
	ctx = domain.WithUnattended(ctx)
	var person domain.UserDetails
	if req.UserID != "" && h.userDetailsLoader != nil {
		if d, err := h.userDetailsLoader(ctx, req.UserID); err == nil {
			person = d
		}
	}
	ctx = domain.WithUserDetails(ctx, person)

	// Everything the agent may look things up with, from all of its abilities.
	st := &wakeState{req: req, loc: loc}
	seen := map[string]bool{}
	var actions []*domain.BoundAction
	for _, skill := range hosted.Agent.Skills {
		tools, err := h.hydrate(hosted, skill)
		if err != nil {
			log.Printf("runtime: plan wake: hydrate skill %q: %v — skipping", skill.Name, err)
			continue
		}
		for _, t := range tools {
			if !t.Spec.ReadOnly || seen[t.Spec.Name] || t.Spec.Name == AskOptionsActionName {
				continue
			}
			seen[t.Spec.Name] = true
			actions = append(actions, st.countLookups(t))
		}
	}
	if len(actions) == 0 {
		return WakePlan{Say: "I'm not connected to anything that could tell me when that will be, so I can't time it. " +
			"Give me a time of day instead, or connect what it depends on."}, nil
	}
	actions = append(actions, buildGetCurrentTimeAction(), st.setNextWake(), st.cannotPlan(), buildFinishAction(false))

	messages := []domain.Message{
		{Role: domain.RoleSystem, Content: wakePrompt(req, loc)},
		{Role: domain.RoleUser, Content: "Work out when this should happen."},
	}
	res, _, err := domain.NewNativeLoop(hosted.Agent.LLMs, maxWakeIterating).Run(ctx, messages, actions)
	if err != nil {
		return WakePlan{}, fmt.Errorf("PlanWake: %w", err)
	}
	if res != nil && res.Status == domain.LoopStatusError && st.plan == nil && st.say == "" {
		return WakePlan{}, fmt.Errorf("PlanWake: %w (%s)", ErrNoWakePlan, res.Content)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.plan != nil:
		return *st.plan, nil
	case st.say != "":
		return WakePlan{Say: st.say}, nil
	}
	return WakePlan{}, ErrNoWakePlan
}

// wakeState is what one planning step has found and decided so far.
type wakeState struct {
	req WakeRequest
	loc *time.Location

	mu      sync.Mutex
	lookups int // look-ups that worked
	plan    *WakePlan
	say     string
}

func wakePrompt(req WakeRequest, loc *time.Location) string {
	var b strings.Builder
	b.WriteString("You decide WHEN a job should next happen. You do not do the job now.\n\n")
	fmt.Fprintf(&b, "The job: %s\n", req.Action)
	fmt.Fprintf(&b, "When the person wants it: %q\n", req.Timing)
	now := req.Now.In(loc)
	fmt.Fprintf(&b, "Right now: %s (%s), which is %s UTC.\n", now.Format("Monday 2006-01-02 15:04"), loc.String(), req.Now.UTC().Format("2006-01-02 15:04"))
	if req.Last != nil {
		fmt.Fprintf(&b, "Earlier you planned to %s at %s because: %s\n", req.Last.Kind, req.Last.At.In(loc).Format("Monday 2006-01-02 15:04"), req.Last.Reason)
		if req.Due {
			b.WriteString("That time has now come.\n")
		}
	}
	b.WriteString(`
How to decide:
1. Find out when the thing the person's words refer to really happens, using your tools. Never answer from memory: dates and times you remember are often out of date, and they move. Times your tools return are usually UTC; convert carefully.
2. Work out the exact moment the job should happen from it (for example "2 hours before" a deadline is the deadline minus 2 hours). Check your sum. If the next one has already passed or is too close, use the one after it.
3. Then call set_next_wake ONCE:
   - kind "act": the exact moment to do the job. You will be woken then and must confirm it still holds, with fresh facts, before it is done.
   - kind "check": a time to look again, because the moment isn't known yet or is far away. Choose it yourself: just before it might change, or soon after something is announced. Never sooner than an hour from now.
   - at: the time with its offset, like 2026-10-25T08:30:00+01:00. Never more than 7 days ahead: if the moment is further off, set a "check" before then.
   - reason: one plain sentence the person will read, naming what you found, for example "The deadline is Saturday 10:30, so two hours before is 8:30."
4. If this is the wake-up of an "act" time and everything you look up still holds: set do_it_now to true, and in the same call give the NEXT wake-up (the next time the job should happen, or when to look for it). If something moved, do not set do_it_now: just give the new time.
5. If you cannot find out with the tools you have, call cannot_plan with one plain sentence for the person: what you couldn't find, and what you'd need from them.
Then call finish. Use plain words everywhere the person might read them; never mention tools, schedules or prompts.`)
	return b.String()
}

// countLookups notes when a look-up really worked: a time that was never looked up is not accepted.
func (st *wakeState) countLookups(t *domain.BoundAction) *domain.BoundAction {
	wrapped := *t
	inner := t.Invoke
	wrapped.Invoke = func(ctx context.Context, input map[string]any) (any, error) {
		out, err := inner(ctx, input)
		if done, _, _ := domain.OutcomeOf(out, err); done {
			st.mu.Lock()
			st.lookups++
			st.mu.Unlock()
		}
		return out, err
	}
	return &wrapped
}

func (st *wakeState) setNextWake() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name: "set_next_wake",
			Description: "Say when the job should next happen (kind act) or when to look again (kind check). " +
				"Call it once, after looking the facts up with your other tools.",
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"at":        {Type: domain.SchemaTypeString, Description: "The time, with its offset, like 2026-10-25T08:30:00+01:00."},
					"kind":      {Type: domain.SchemaTypeString, Description: "act (do the job then) or check (look again then).", Enum: []string{"act", "check"}},
					"reason":    {Type: domain.SchemaTypeString, Description: "One plain sentence for the person: what you found and how you got this time."},
					"do_it_now": {Type: domain.SchemaTypeBoolean, Description: "Only when woken at an act time and it still holds: do the job now. Then at/kind/reason are for the NEXT one."},
				},
				Required: []string{"at", "kind", "reason"},
			},
			MaxCalls: 3,
		},
		Invoke: func(_ context.Context, input map[string]any) (any, error) {
			plan, err := st.validate(input)
			if err != nil {
				return nil, err
			}
			st.mu.Lock()
			st.plan = &plan
			st.mu.Unlock()
			return map[string]any{"message": "Noted. Now call finish."}, nil
		},
	}
}

// validate holds the agent's answer to the few things the code insists on. A refusal says what to
// change, so the agent can fix it and call again.
func (st *wakeState) validate(input map[string]any) (WakePlan, error) {
	st.mu.Lock()
	lookups := st.lookups
	st.mu.Unlock()
	if lookups == 0 {
		return WakePlan{}, errors.New("look it up first: use your tools to find out when it happens, then set the time. A time from memory is not accepted")
	}
	str := func(k string) string { s, _ := input[k].(string); return strings.TrimSpace(s) }

	kind := WakeKind(strings.ToLower(str("kind")))
	if kind != WakeAct && kind != WakeCheck {
		return WakePlan{}, errors.New(`kind must be "act" or "check"`)
	}
	reason := str("reason")
	if reason == "" {
		return WakePlan{}, errors.New("give the reason in one plain sentence")
	}
	if utf8.RuneCountInString(reason) > maxWakeReason {
		reason = string([]rune(reason)[:maxWakeReason])
	}
	at, err := parseWakeTime(str("at"), st.loc)
	if err != nil {
		return WakePlan{}, err
	}

	now := st.req.Now
	min := MinActAhead
	if kind == WakeCheck {
		min = MinCheckAhead
	}
	switch {
	case at.Before(now.Add(min)):
		return WakePlan{}, fmt.Errorf("that is too soon: a %q wake-up must be at least %d minutes from now (now is %s). If the moment is nearer than that, use the one after it",
			kind, int(min/time.Minute), now.UTC().Format(time.RFC3339))
	case at.After(now.Add(MaxWakeAhead)):
		return WakePlan{}, fmt.Errorf("that is too far ahead: choose a time no later than %s, and use kind \"check\" to look again then",
			now.Add(MaxWakeAhead).UTC().Format(time.RFC3339))
	}

	doIt, _ := input["do_it_now"].(bool)
	if doIt {
		switch {
		case !st.req.Due || st.req.Last == nil || st.req.Last.Kind != WakeAct:
			return WakePlan{}, errors.New("it is not the moment: do_it_now is only for the wake-up of an act time")
		case now.Sub(st.req.Last.At) > LateWake:
			return WakePlan{}, errors.New("the moment has passed too long ago to do it now: give the next time instead, without do_it_now")
		}
	}
	return WakePlan{At: at, Kind: kind, Reason: reason, DoItNow: doIt}, nil
}

func parseWakeTime(s string, loc *time.Location) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("give the time, like 2026-10-25T08:30:00+01:00")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use the form 2026-10-25T08:30:00+01:00", s)
}

func (st *wakeState) cannotPlan() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        "cannot_plan",
			Description: "Use this only when, with the tools you have, you cannot find out when the job should happen. Say it in one plain sentence to the person: what you couldn't find and what you'd need.",
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"message": {Type: domain.SchemaTypeString, Description: "One plain sentence for the person."},
				},
				Required: []string{"message"},
			},
			MaxCalls: 1,
		},
		Invoke: func(_ context.Context, input map[string]any) (any, error) {
			msg, _ := input["message"].(string)
			msg = strings.TrimSpace(msg)
			if msg == "" {
				return nil, errors.New("say what you couldn't find, in one plain sentence")
			}
			if utf8.RuneCountInString(msg) > maxWakeSay {
				msg = string([]rune(msg)[:maxWakeSay])
			}
			st.mu.Lock()
			st.say = msg
			st.mu.Unlock()
			return map[string]any{"message": "Noted. Now call finish."}, nil
		},
	}
}

// Notify tells a person something in their chat when they have not asked: a task of theirs that had
// to stop, for example. It reports whether it could be delivered to a known chat.
func (h *Host) Notify(ctx context.Context, agentID, userID, identityID, channelRef, text string) error {
	h.mu.RLock()
	hosted, ok := h.agents[agentID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("Notify: agent %q not registered", agentID)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("Notify: nothing to say")
	}
	conv := domain.ConversationRef{IdentityID: identityID, ChatID: channelRef, UserID: userID, SenderID: channelRef}
	if identity, integration := resolveIdentityAndIntegration(hosted, conv); identity != nil && integration != nil {
		conv.Provider = integration.Service
	}
	if h.deliverBestEffortID(ctx, hosted, conv, text) == "" {
		return errors.New("Notify: could not be delivered")
	}
	return nil
}
