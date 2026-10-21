package domain

import (
	"net/http"
	"strings"
	"testing"
)

func summaryEvent(summary string) *IntegrationEvent {
	return &IntegrationEvent{
		Name: "svc.thing.happened", Description: "Something happened", Arrival: ArrivalPushed,
		Handler: func([]byte, http.Header) (string, string, bool, error) { return "", "", false, nil },
		Subject: EventSubject{Kind: SubjectPayload, Field: "who", Account: "svc_account"},
		Payload: Schema{Type: SchemaTypeObject, Properties: map[string]Schema{
			"who":   {Type: SchemaTypeString, Description: "who"},
			"id":    {Type: SchemaTypeString, Description: "id"},
			"title": {Type: SchemaTypeString, Description: "title"},
			"count": {Type: SchemaTypeInteger, Description: "count"},
			"note":  {Type: SchemaTypeString, Description: "optional note"},
			"tags":  {Type: SchemaTypeArray, Description: "tags", Items: &Schema{Type: SchemaTypeString}},
		}, Required: []string{"who", "id", "title", "count"}},
		RepeatKey: "id",
		Sample:    map[string]any{"who": "octo", "id": "1", "title": "Fix it", "count": 3},
		Summary:   summary,
	}
}

func TestSummary_IsOptionalButHeldToTheContractWhenThere(t *testing.T) {
	if err := ValidateEvent("svc", summaryEvent("")); err != nil {
		t.Fatalf("no summary is fine: %v", err)
	}
	if err := ValidateEvent("svc", summaryEvent("New thing from {{who}}: {{title}} ({{count}})")); err != nil {
		t.Fatalf("a good one: %v", err)
	}
	cases := map[string]string{
		"   ":                                "only spaces",
		"Something happened":                 "no blanks",
		"From {{nobody}}":                    "doesn't have",
		"Note: {{note}}":                     "not always there",
		"From {{who}} {{ oops":               "not written {{name}}",
		"A webhook from {{who}}":             "platform word",
		"Line one\nline two {{who}}":         "one line",
		strings.Repeat("x", 300) + "{{who}}": "most allowed",
	}
	for summary, want := range cases {
		err := ValidateEvent("svc", summaryEvent(summary))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", summary, err)
		}
	}
}

func TestSummary_AListIsNotAPlainValueEvenWhenRequired(t *testing.T) {
	e := summaryEvent("Tags: {{tags}}")
	e.Payload.Required = append(e.Payload.Required, "tags")
	e.Sample["tags"] = []any{"a"}
	if err := ValidateEvent("svc", e); err == nil || !strings.Contains(err.Error(), "not a plain value") {
		t.Fatalf("%v", err)
	}
}

func TestRenderSummary_FillsBlanksSafely(t *testing.T) {
	e := summaryEvent("New thing from {{who}}: {{title}} ({{count}})")
	got, err := e.RenderSummary(e.Sample)
	if err != nil || got != "New thing from octo: Fix it (3)" {
		t.Fatalf("%q %v", got, err)
	}
	// Numbers read from a delivery are floats; whole ones are written whole.
	got, _ = e.RenderSummary(map[string]any{"who": "octo", "title": "T", "count": float64(12)})
	if got != "New thing from octo: T (12)" {
		t.Errorf("%q", got)
	}
	got, _ = e.RenderSummary(map[string]any{"who": "octo", "title": "Line one\nline  two\t\x07bell " + strings.Repeat("y", 300), "count": 1})
	if strings.ContainsAny(got, "\n\t\x07") || strings.Contains(got, "  ") || len([]rune(got)) > 60+MaxSummaryValueLen {
		t.Errorf("a title from outside is one tidy, short line: %q", got)
	}
	if _, err := e.RenderSummary(map[string]any{"who": "octo", "count": 1}); err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("a missing fact is an error, not an empty blank: %v", err)
	}
	if _, err := summaryEvent("").RenderSummary(e.Sample); err == nil {
		t.Error("no summary to render")
	}
}
