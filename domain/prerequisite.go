package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// A capability's creator can make one of its abilities wait for another: "check for open trades on the pair before
// placing a trade". Written as an instruction it is usually followed, never guaranteed; as a Prerequisite it is kept by
// the platform. The later ability is refused, back to the model, until the earlier one has run successfully in the same
// run, for the same values of the inputs named in Same (the same pair). The model is told what to run first, does it,
// and tries again. Nothing is approved or sent before then.

// Prerequisite is one such rule, on a Skill.
type Prerequisite struct {
	ID     string   `json:"id,omitempty"`
	Before string   `json:"before"`         // the ability that waits (a tool id)
	First  string   `json:"first"`          // the ability that must have run first (a tool id); it only looks things up
	Same   []string `json:"same,omitempty"` // inputs both have that must carry the same value ("symbol")
	Text   string   `json:"text,omitempty"` // how the creator saw it, in plain words
}

// MaxPrerequisites bounds a capability's prerequisites.
const MaxPrerequisites = 10

// NormalizePrerequisites checks a capability's prerequisites against the abilities it grants (by tool id): both must be
// among them, the first must only look things up (a rule never makes the agent change something), and every input
// named in Same must exist on both. It drops repeats and returns the rules tidied, or what is wrong in plain words.
func NormalizePrerequisites(ps []Prerequisite, granted map[string]*ToolDefinition) ([]Prerequisite, error) {
	if len(ps) > MaxPrerequisites {
		return nil, fmt.Errorf("a capability can keep at most %d of these", MaxPrerequisites)
	}
	seen := map[string]bool{}
	var out []Prerequisite
	for _, p := range ps {
		before, ok := granted[p.Before]
		if !ok {
			return nil, fmt.Errorf("%q isn't something this capability can do", p.Before)
		}
		first, ok := granted[p.First]
		if !ok {
			return nil, fmt.Errorf("%q isn't something this capability can do", p.First)
		}
		if p.Before == p.First {
			return nil, fmt.Errorf("something can't have to happen before itself")
		}
		if !first.ReadOnly {
			return nil, fmt.Errorf("only something that looks things up can be made to happen first (%s changes things)", first.ModelName())
		}
		var same []string
		for _, s := range p.Same {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			_, inBefore := before.InputSchema.Properties[s]
			_, inFirst := first.InputSchema.Properties[s]
			if !inBefore || !inFirst {
				return nil, fmt.Errorf("%s and %s don't both have %q", before.ModelName(), first.ModelName(), s)
			}
			same = append(same, s)
		}
		key := p.Before + "|" + p.First + "|" + strings.Join(same, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		p.Same = same
		p.Text = strings.TrimSpace(p.Text)
		if len([]rune(p.Text)) > 200 {
			p.Text = string([]rune(p.Text)[:200])
		}
		out = append(out, p)
	}
	return out, nil
}

// --- what ran in this run ---

// RunJournal is what ran successfully in one run, with what it was given: what a prerequisite is checked against. A run
// gets one when its loop starts; nested runs (a capability that runs its own steps) share their caller's.
type RunJournal struct {
	mu   sync.Mutex
	done []journalEntry
}

type journalEntry struct {
	tool  string
	input map[string]any
}

type runJournalKey struct{}

// WithRunJournal gives the run a journal, unless it already has one.
func WithRunJournal(ctx context.Context) context.Context {
	if RunJournalFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, runJournalKey{}, &RunJournal{})
}

// RunJournalFrom is the run's journal, nil when it has none.
func RunJournalFrom(ctx context.Context) *RunJournal {
	j, _ := ctx.Value(runJournalKey{}).(*RunJournal)
	return j
}

// Note writes down that a tool ran successfully with this input.
func (j *RunJournal) Note(toolID string, input map[string]any) {
	if j == nil {
		return
	}
	copied := make(map[string]any, len(input))
	for k, v := range input {
		copied[k] = v
	}
	j.mu.Lock()
	j.done = append(j.done, journalEntry{tool: toolID, input: copied})
	j.mu.Unlock()
}

// Has reports whether a tool ran successfully with the same values as input for each of same.
func (j *RunJournal) Has(toolID string, same []string, input map[string]any) bool {
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, e := range j.done {
		if e.tool != toolID {
			continue
		}
		all := true
		for _, s := range same {
			if !sameInput(e.input[s], input[s]) {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// sameInput compares two inputs the way a person would: text ignoring case and spaces at the ends, numbers as numbers.
func sameInput(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.EqualFold(strings.TrimSpace(as), strings.TrimSpace(bs))
	}
	af, aNum := numberOfValue(a)
	bf, bNum := numberOfValue(b)
	if aNum && bNum {
		return af == bf
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func numberOfValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// withJournal writes down each successful run of a tool in the run's journal. It sits right around the service call, so
// only what really ran counts: a call refused by a gate, declined or failed is not written down.
func withJournal(toolID string, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		out, err := inner(ctx, input)
		if done, _, _ := OutcomeOf(out, err); done && !saysItFailed(out) {
			RunJournalFrom(ctx).Note(toolID, input)
		}
		return out, err
	}
}

// saysItFailed reports whether a result that came back without an error still says it didn't work: an error or refusal
// status, ok false, or an error message. A check like that has not been done.
func saysItFailed(out any) bool {
	m, ok := out.(map[string]any)
	if !ok {
		return false
	}
	switch strings.ToLower(fmt.Sprint(m["status"])) {
	case "error", "failed", "failure", "refused", "denied":
		return true
	}
	if okv, isBool := m["ok"].(bool); isBool && !okv {
		return true
	}
	if e, isStr := m["error"].(string); isStr && strings.TrimSpace(e) != "" {
		return true
	}
	return false
}

// WrapPrerequisites makes an ability wait for the abilities its capability says must run first. Called on it, before
// anything else (the input check, the approval), it is refused back to the model, saying what to run first and for
// which values, until each has run successfully in this run. names maps a tool id to the name the model calls it by.
func WrapPrerequisites(action *BoundAction, rules []Prerequisite, names map[string]string) *BoundAction {
	if len(rules) == 0 {
		return action
	}
	inner := action.Invoke
	wrapped := *action
	for _, r := range rules {
		wrapped.Needs = append(wrapped.Needs, names[r.First])
	}
	wrapped.Invoke = func(ctx context.Context, input map[string]any) (any, error) {
		j := RunJournalFrom(ctx)
		for _, r := range rules {
			if j.Has(r.First, r.Same, input) {
				continue
			}
			first := names[r.First]
			msg := fmt.Sprintf("Not done: before %s, run %s first", action.Spec.Name, first)
			if len(r.Same) > 0 {
				var vals []string
				for _, s := range r.Same {
					vals = append(vals, fmt.Sprintf("%s %v", s, input[s]))
				}
				msg += " for the same " + strings.Join(vals, ", ")
			}
			msg += ", as required. Do that now, then call " + action.Spec.Name + " again if it still makes sense."
			return map[string]any{"status": "refused", "changed": false, "message": msg}, nil
		}
		return inner(ctx, input)
	}
	return &wrapped
}
