package domain

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxEventSummaryLen keeps the template to a line or two.
	MaxEventSummaryLen = 280
	// MaxSummaryValueLen cuts any one value placed in a message: a title or a note comes from outside and can be long.
	MaxSummaryValueLen = 200
)

var summaryBlank = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_]*)\s*\}\}`)

// summaryProblems says how an event's Summary breaks the contract. No Summary is fine.
func summaryProblems(e *IntegrationEvent) []string {
	if strings.TrimSpace(e.Summary) == "" {
		if e.Summary != "" {
			return []string{"the summary is only spaces"}
		}
		return nil
	}
	var out []string
	if utf8.RuneCountInString(e.Summary) > MaxEventSummaryLen {
		out = append(out, fmt.Sprintf("the summary is %d characters; the most allowed is %d", utf8.RuneCountInString(e.Summary), MaxEventSummaryLen))
	}
	if strings.ContainsAny(e.Summary, "\n\r") {
		out = append(out, "the summary must be one line")
	}
	blanks := summaryBlank.FindAllStringSubmatch(e.Summary, -1)
	if len(blanks) == 0 {
		out = append(out, "the summary has no blanks, so it would say the same thing every time")
	}
	required := map[string]bool{}
	for _, r := range e.Payload.Required {
		required[r] = true
	}
	for _, b := range blanks {
		p, ok := e.Payload.Properties[b[1]]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("the summary uses {{%s}}, which the payload doesn't have", b[1]))
		case !required[b[1]]:
			out = append(out, fmt.Sprintf("the summary uses {{%s}}, which is not always there (it must be required)", b[1]))
		case p.Type != SchemaTypeString && p.Type != SchemaTypeInteger && p.Type != SchemaTypeNumber && p.Type != SchemaTypeBoolean:
			out = append(out, fmt.Sprintf("the summary uses {{%s}}, which is not a plain value", b[1]))
		}
	}
	rest := summaryBlank.ReplaceAllString(e.Summary, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		out = append(out, "the summary has a blank that is not written {{name}}")
	}
	if w := platformWords.FindString(rest); w != "" {
		out = append(out, fmt.Sprintf("the summary uses a platform word (%q); say it in plain words", w))
	}
	return out
}

// RenderSummary fills in the event's Summary from the facts of one occurrence, for a person to read. It is an error
// when the event has no Summary or a fact it needs is missing. Values come from outside (a title, a note) so they are
// made safe for a one-line message: whitespace is collapsed, control characters removed, and each is cut short.
func (e *IntegrationEvent) RenderSummary(facts map[string]any) (string, error) {
	if strings.TrimSpace(e.Summary) == "" {
		return "", fmt.Errorf("event %q has no summary", e.Name)
	}
	var missing []string
	out := summaryBlank.ReplaceAllStringFunc(e.Summary, func(blank string) string {
		name := summaryBlank.FindStringSubmatch(blank)[1]
		v, ok := facts[name]
		if !ok || v == nil {
			missing = append(missing, name)
			return ""
		}
		return plainValue(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("event %q: the summary needs %s", e.Name, strings.Join(missing, ", "))
	}
	return out, nil
}

// plainValue writes a fact on one line, without control characters, cut to MaxSummaryValueLen.
func plainValue(v any) string {
	var s string
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			s = fmt.Sprintf("%d", int64(x))
		} else {
			s = fmt.Sprint(x)
		}
	default:
		s = fmt.Sprint(v)
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxSummaryValueLen {
		s = string(r[:MaxSummaryValueLen]) + "…"
	}
	return s
}
