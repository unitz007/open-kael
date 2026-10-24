package domain

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ── Rules a creator sets on how an agent may use a tool ───────────────────────
//
// A rule belongs to one agent and one tool, and says something about one of that tool's
// inputs. It is held by the agent, not by the service behind the tool, so it works the same for
// every integration (built in or the creator's own) and the service never has to know about it.
//
//	max / min      the largest or smallest number allowed
//	one_of         only these values
//	none_of        never these values
//	max_product    two number inputs multiplied together may not come to more than this (a trade's
//	               size times its price is its value)
//	fixed          always this value; the model never sees the input and can't change it
//	default        this value when the model gives none
//
// A call that breaks max, min, one_of or none_of is refused before anyone is asked to approve it
// and before the service sees it, with a reason in plain words and "nothing was changed".

// RuleKind says what a rule does.
type RuleKind string

const (
	RuleMax RuleKind = "max"
	// RuleMaxProduct limits Input × With: the two details together, not either alone.
	RuleMaxProduct RuleKind = "max_product"
	RuleMin        RuleKind = "min"
	RuleOneOf      RuleKind = "one_of"
	RuleNoneOf     RuleKind = "none_of"
	RuleFixed      RuleKind = "fixed"
	RuleDefault    RuleKind = "default"
)

const (
	maxRuleValues   = 50
	maxRuleMessage  = 200
	maxRulesPerTool = 20
)

// ToolRule is one rule on one input of one tool, for one agent.
type ToolRule struct {
	ID     string `json:"id,omitempty"`
	ToolID string `json:"tool_id"`
	Input  string `json:"input"`
	// With is the second input of a max_product rule (Input × With). Empty for every other kind.
	With string   `json:"with,omitempty"`
	Kind RuleKind `json:"kind"`
	// Value is a number (max, min), a list of values (one_of, none_of) or a single value
	// (fixed, default), of the kind the input takes.
	Value any `json:"value"`
	// Message is the creator's own wording for a refusal, in plain words. Empty means a
	// standard sentence is used.
	Message string `json:"message,omitempty"`
}

// RulesFor returns the agent's rules for one tool.
func (a *Agent) RulesFor(toolID string) []ToolRule {
	if a == nil {
		return nil
	}
	var out []ToolRule
	for _, r := range a.Rules {
		if r.ToolID == toolID {
			out = append(out, r)
		}
	}
	return out
}

// NormalizeRule checks a rule against the tool it is for and returns it in its canonical form
// (numbers as numbers, values of the input's own type). The error is written for the creator.
func NormalizeRule(r ToolRule, def *ToolDefinition) (ToolRule, error) {
	if def == nil {
		return r, fmt.Errorf("that isn't something it can do")
	}
	r.Input = strings.TrimSpace(r.Input)
	r.Message = strings.TrimSpace(r.Message)
	if utf8.RuneCountInString(r.Message) > maxRuleMessage {
		return r, fmt.Errorf("the message is too long (up to %d characters)", maxRuleMessage)
	}
	prop, ok := def.detail(r.Input)
	if !ok {
		return r, fmt.Errorf("%q isn't a detail of %s", r.Input, def.Name)
	}
	label := humanize(r.Input)
	if r.Kind != RuleMaxProduct {
		r.With = ""
	}
	switch r.Kind {
	case RuleMaxProduct:
		r.With = strings.TrimSpace(r.With)
		other, ok := def.detail(r.With)
		if !ok {
			return r, fmt.Errorf("%q isn't a detail of %s", r.With, def.Name)
		}
		if r.With == r.Input {
			return r, fmt.Errorf("pick two different details to multiply")
		}
		for _, p := range []struct {
			name string
			s    Schema
		}{{r.Input, prop}, {r.With, other}} {
			if p.s.Type != SchemaTypeNumber && p.s.Type != SchemaTypeInteger {
				return r, fmt.Errorf("%s isn't a number, so it can't be multiplied", humanize(p.name))
			}
		}
		n, ok := toNumber(r.Value)
		if !ok || n <= 0 {
			return r, fmt.Errorf("the limit for %s × %s must be a number above zero", label, humanize(r.With))
		}
		r.Value = n
	case RuleMax, RuleMin:
		if prop.Type != SchemaTypeNumber && prop.Type != SchemaTypeInteger {
			return r, fmt.Errorf("%s isn't a number, so it can't have a largest or smallest allowed", label)
		}
		n, ok := toNumber(r.Value)
		if !ok {
			return r, fmt.Errorf("the limit for %s must be a number", label)
		}
		r.Value = n
	case RuleOneOf, RuleNoneOf:
		list, ok := r.Value.([]any)
		if !ok {
			if strs, isStrs := r.Value.([]string); isStrs {
				for _, s := range strs {
					list = append(list, s)
				}
				ok = true
			}
		}
		if !ok || len(list) == 0 || len(list) > maxRuleValues {
			return r, fmt.Errorf("give between 1 and %d values for %s", maxRuleValues, label)
		}
		canon := make([]any, 0, len(list))
		for _, v := range list {
			cv, err := checkRuleValue(prop, v)
			if err != nil {
				return r, fmt.Errorf("%v isn't a valid value for %s: %w", v, label, err)
			}
			if r.Kind == RuleOneOf && len(prop.Enum) > 0 && !enumHas(prop.Enum, cv) {
				return r, fmt.Errorf("%v isn't one of the values %s can take", v, label)
			}
			canon = append(canon, cv)
		}
		r.Value = canon
	case RuleFixed, RuleDefault:
		cv, err := checkRuleValue(prop, r.Value)
		if err != nil {
			return r, fmt.Errorf("that isn't a valid value for %s: %w", label, err)
		}
		if len(prop.Enum) > 0 && !enumHas(prop.Enum, cv) {
			return r, fmt.Errorf("that isn't one of the values %s can take", label)
		}
		r.Value = cv
	default:
		return r, fmt.Errorf("unknown kind of rule %q", r.Kind)
	}
	return r, nil
}

// NormalizeRules checks a whole set of rules for one agent against the tools they name, and
// returns them in canonical form. Rules that can't both hold are refused.
func NormalizeRules(rules []ToolRule, tools map[string]*ToolDefinition) ([]ToolRule, error) {
	out := make([]ToolRule, 0, len(rules))
	perTool := map[string]int{}
	type slot struct{ tool, input string }
	pinned := map[slot]RuleKind{}
	products := map[[3]string]bool{}
	maxes, mins := map[slot]float64{}, map[slot]float64{}
	for _, r := range rules {
		n, err := NormalizeRule(r, tools[r.ToolID])
		if err != nil {
			return nil, err
		}
		if perTool[n.ToolID]++; perTool[n.ToolID] > maxRulesPerTool {
			return nil, fmt.Errorf("too many rules on one thing it does (up to %d)", maxRulesPerTool)
		}
		s := slot{n.ToolID, n.Input}
		switch n.Kind {
		case RuleFixed, RuleDefault:
			if prev, dup := pinned[s]; dup {
				return nil, fmt.Errorf("%s already has a %s value; a detail can have only one", humanize(n.Input), strings.ReplaceAll(string(prev), "_", " "))
			}
			pinned[s] = n.Kind
		case RuleMaxProduct:
			key := [3]string{n.ToolID, n.Input, n.With}
			rev := [3]string{n.ToolID, n.With, n.Input}
			if products[key] || products[rev] {
				return nil, fmt.Errorf("%s × %s already has a limit", humanize(n.Input), humanize(n.With))
			}
			products[key] = true
		case RuleMax:
			if _, dup := maxes[s]; dup {
				return nil, fmt.Errorf("%s already has a largest allowed", humanize(n.Input))
			}
			maxes[s] = n.Value.(float64)
		case RuleMin:
			if _, dup := mins[s]; dup {
				return nil, fmt.Errorf("%s already has a smallest allowed", humanize(n.Input))
			}
			mins[s] = n.Value.(float64)
		}
		out = append(out, n)
	}
	for s, hi := range maxes {
		if lo, ok := mins[s]; ok && lo > hi {
			return nil, fmt.Errorf("the smallest allowed for %s is bigger than the largest allowed", humanize(s.input))
		}
	}
	return out, nil
}

// checkRuleValue converts v to the kind of value the input takes.
func checkRuleValue(prop Schema, v any) (any, error) {
	switch prop.Type {
	case SchemaTypeString, SchemaTypeNumber, SchemaTypeInteger, SchemaTypeBoolean:
		return prop.check(v)
	}
	return nil, fmt.Errorf("rules can only be set on text, number, yes/no details")
}

// ── The gate ──────────────────────────────────────────────────────────────────

// applyPins fills in the values the creator fixed or defaulted, before the call is checked.
func applyPins(rules []ToolRule, input map[string]any) map[string]any {
	pinned := false
	for _, r := range rules {
		if r.Kind == RuleFixed || r.Kind == RuleDefault {
			pinned = true
		}
	}
	if !pinned {
		return input
	}
	out := make(map[string]any, len(input)+2)
	for k, v := range input {
		out[k] = v
	}
	for _, r := range rules {
		switch r.Kind {
		case RuleFixed:
			out[r.Input] = r.Value
		case RuleDefault:
			if v, ok := out[r.Input]; !ok || v == nil || v == "" {
				out[r.Input] = r.Value
			}
		}
	}
	return out
}

// breaks returns the reason, in plain words, a call breaks a rule ("" when it keeps them all).
func breaks(rules []ToolRule, input map[string]any) string {
	for _, r := range rules {
		if r.Kind == RuleMaxProduct {
			if why := breaksProduct(r, input); why != "" {
				return why
			}
			continue
		}
		v, present := input[r.Input]
		if !present || v == nil {
			continue
		}
		switch r.Kind {
		case RuleMax:
			if n, ok := toNumber(v); ok && n > r.Value.(float64) {
				return refusalText(r, fmt.Sprintf("That's more than the most allowed for %s (%s), so nothing was done.", humanize(r.Input), formatNumber(r.Value.(float64))))
			}
		case RuleMin:
			if n, ok := toNumber(v); ok && n < r.Value.(float64) {
				return refusalText(r, fmt.Sprintf("That's less than the least allowed for %s (%s), so nothing was done.", humanize(r.Input), formatNumber(r.Value.(float64))))
			}
		case RuleOneOf:
			if !valueIn(r.Value, v) {
				return refusalText(r, fmt.Sprintf("Only these are allowed for %s: %s. So nothing was done.", humanize(r.Input), listOf(r.Value)))
			}
		case RuleNoneOf:
			if valueIn(r.Value, v) {
				return refusalText(r, fmt.Sprintf("%s isn't allowed for %s, so nothing was done.", capitalFirst(fmt.Sprint(v)), humanize(r.Input)))
			}
		}
	}
	return ""
}

// breaksProduct checks Input × With against its limit. A limit that can't be checked is not a limit,
// so a call missing either number is refused too (the model is told to give both).
func breaksProduct(r ToolRule, input map[string]any) string {
	a, okA := toNumber(input[r.Input])
	b, okB := toNumber(input[r.With])
	what := humanize(r.Input) + " × " + humanize(r.With)
	if !okA || !okB {
		return refusalText(r, fmt.Sprintf("I can't check the limit on %s without both numbers, so nothing was done.", what))
	}
	limit := r.Value.(float64)
	total := a * b
	if total-limit > 1e-9*math.Max(1, math.Abs(limit)) {
		return refusalText(r, fmt.Sprintf("That comes to %s for %s, which is more than the most allowed (%s), so nothing was done.", formatNumber(roundTo(total, 2)), what, formatNumber(limit)))
	}
	return ""
}

func roundTo(n float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(n*p) / p
}

func refusalText(r ToolRule, standard string) string {
	if r.Message != "" {
		return r.Message
	}
	return standard
}

// withPins fills in fixed and default values first, so the input check sees a whole call.
func withPins(rules []ToolRule, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		return inner(ctx, applyPins(rules, input))
	}
}

// withRules refuses a call that breaks a rule, in the shape every refusal has ("changed": false),
// so the agent can't say it did something and the person is told why.
func withRules(rules []ToolRule, name string, inner func(ctx context.Context, input map[string]any) (any, error)) func(ctx context.Context, input map[string]any) (any, error) {
	return func(ctx context.Context, input map[string]any) (any, error) {
		if why := breaks(rules, input); why != "" {
			logRuleRefusal(name, why)
			return map[string]any{"status": "refused", "changed": false, "message": why}, nil
		}
		return inner(ctx, input)
	}
}

// specWithRules is what the model is shown: fixed details are gone (it can't set them), defaulted
// ones are optional, and each limit is stated so it doesn't make calls that are bound to be refused.
func specWithRules(spec ToolSpec, rules []ToolRule) ToolSpec {
	if len(rules) == 0 {
		return spec
	}
	props := make(map[string]Schema, len(spec.Input.Properties))
	for k, v := range spec.Input.Properties {
		props[k] = v
	}
	required := append([]string(nil), spec.Input.Required...)
	drop := func(name string) {
		kept := required[:0]
		for _, n := range required {
			if n != name {
				kept = append(kept, n)
			}
		}
		required = kept
	}
	note := func(name, text string) {
		p, ok := props[name]
		if !ok {
			return
		}
		p.Description = strings.TrimSpace(p.Description + " " + text)
		props[name] = p
	}
	for _, r := range rules {
		switch r.Kind {
		case RuleFixed:
			delete(props, r.Input)
			drop(r.Input)
		case RuleDefault:
			drop(r.Input)
			note(r.Input, fmt.Sprintf("If you leave it out, %s is used.", fmt.Sprint(r.Value)))
		case RuleMaxProduct:
			limit := formatNumber(r.Value.(float64))
			note(r.Input, fmt.Sprintf("Together with %s, %s × %s may not come to more than %s.", humanize(r.With), humanize(r.Input), humanize(r.With), limit))
			note(r.With, fmt.Sprintf("Together with %s, %s × %s may not come to more than %s.", humanize(r.Input), humanize(r.Input), humanize(r.With), limit))
		case RuleMax:
			note(r.Input, fmt.Sprintf("Allowed: at most %s.", formatNumber(r.Value.(float64))))
		case RuleMin:
			note(r.Input, fmt.Sprintf("Allowed: at least %s.", formatNumber(r.Value.(float64))))
		case RuleOneOf:
			note(r.Input, fmt.Sprintf("Allowed: only %s.", listOf(r.Value)))
		case RuleNoneOf:
			note(r.Input, fmt.Sprintf("Not allowed: %s.", listOf(r.Value)))
		}
	}
	spec.Input.Properties = props
	spec.Input.Required = required
	return spec
}

func logRuleRefusal(tool, why string) {
	log.Printf("rules: refused a call to %s: %s", tool, why)
}

// ── small helpers ─────────────────────────────────────────────────────────────

func toNumber(v any) (float64, bool) {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case float32:
		n = float64(x)
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		n = f
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

func valueIn(list any, v any) bool {
	items, _ := list.([]any)
	for _, it := range items {
		if sameValue(it, v) {
			return true
		}
	}
	return false
}

func sameValue(a, b any) bool {
	if x, ok := toNumber(a); ok {
		if y, ok := toNumber(b); ok {
			return x == y
		}
	}
	if x, ok := a.(bool); ok {
		y, ok := b.(bool)
		return ok && x == y
	}
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(a)), strings.TrimSpace(fmt.Sprint(b)))
}

func enumHas(enum []string, v any) bool {
	for _, e := range enum {
		if sameValue(e, v) {
			return true
		}
	}
	return false
}

func listOf(list any) string {
	items, _ := list.([]any)
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if n, ok := it.(float64); ok {
			parts = append(parts, formatNumber(n))
		} else {
			parts = append(parts, fmt.Sprint(it))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func formatNumber(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func humanize(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "_", " ")
}

func capitalFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}
