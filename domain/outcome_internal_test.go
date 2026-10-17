package domain

import (
	"strings"
	"testing"
)

func TestDescribeRecords_IsAOneLineSummaryOfWhatReallyRan(t *testing.T) {
	if got := describeRecords(nil); got != "nothing" {
		t.Errorf("nothing ran: %q", got)
	}
	got := describeRecords([]ActionRecord{
		{Name: "get_quote", Done: true},
		{Name: "place_trade", Done: false, Reason: "you need to connect your account first"},
	})
	if !strings.Contains(got, "get_quote=done") || !strings.Contains(got, "place_trade=NOT done (you need to connect your account first)") || strings.Contains(got, "\n") {
		t.Errorf("summary: %q", got)
	}
}
