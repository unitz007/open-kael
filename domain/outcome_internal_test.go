package domain

import (
	"context"
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

// The incident: the broker refused a trade without raising an error, the once-per-turn
// guard counted that as the one allowed success, then answered the agent's retry with
// "already completed" — and the turn was reported done with nothing placed.
func TestOneShotGuard_OnlyARealSuccessUsesUpTheShot(t *testing.T) {
	refused := map[string]any{"changed": false, "message": "RR 1.89 is below the minimum you set (2.00)"}
	placed := map[string]any{"changed": true, "message": "Bought 10000 EUR/USD"}
	calls := 0
	outputs := []any{refused, OutputNotApproved, OutputAuthPendingPrefix + " connect", placed}
	guard := withOneShotGuard(func(_ context.Context, _ map[string]any) (any, error) {
		out := outputs[calls]
		calls++
		return out, nil
	})
	for i := range outputs {
		out, _ := guard(context.Background(), nil)
		if s, _ := out.(string); strings.HasPrefix(s, "already completed") {
			t.Fatalf("call %d was answered 'already completed' though nothing had succeeded", i+1)
		}
	}
	if calls != 4 {
		t.Fatalf("refusals must leave the action free to be tried again: ran %d of 4", calls)
	}
	out, _ := guard(context.Background(), nil)
	if s, _ := out.(string); !strings.HasPrefix(s, "already completed") || calls != 4 {
		t.Errorf("after a real success the action does not run again: %v (ran %d)", out, calls)
	}
}

func TestChangeFailed(t *testing.T) {
	read := ActionRecord{Name: "list", Changes: false, Done: false}
	failed := ActionRecord{Name: "create", Changes: true, Done: false}
	worked := ActionRecord{Name: "create", Changes: true, Done: true}
	for name, tc := range map[string]struct {
		in   []ActionRecord
		want bool
	}{
		"nothing ran":        {nil, false},
		"only a read failed": {[]ActionRecord{read}, false},
		"a change failed":    {[]ActionRecord{read, failed}, true},
		"failed twice":       {[]ActionRecord{failed, failed}, true},
		"failed then worked": {[]ActionRecord{failed, worked}, false},
		"worked":             {[]ActionRecord{worked}, false},
	} {
		if got := ChangeFailed(tc.in); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}
