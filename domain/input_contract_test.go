package domain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func obj(required []string, props map[string]domain.Schema) domain.Schema {
	return domain.Schema{Type: domain.SchemaTypeObject, Required: required, Properties: props}
}

func TestCheckInput_ConvertsHarmlessDifferencesAndKeepsTheRest(t *testing.T) {
	s := obj([]string{"symbol"}, map[string]domain.Schema{
		"symbol": {Type: domain.SchemaTypeString},
		"amount": {Type: domain.SchemaTypeNumber},
		"count":  {Type: domain.SchemaTypeInteger},
		"live":   {Type: domain.SchemaTypeBoolean},
		"tags":   {Type: domain.SchemaTypeArray, Items: &domain.Schema{Type: domain.SchemaTypeString}},
		"side":   {Type: domain.SchemaTypeString, Enum: []string{"buy", "sell"}},
		"note":   {Type: domain.SchemaTypeString},
		"opts":   obj([]string{"deep"}, map[string]domain.Schema{"deep": {Type: domain.SchemaTypeInteger}}),
	})
	in := map[string]any{
		"symbol": "EURUSD", "amount": "1.5", "count": 5.0, "live": "TRUE",
		"tags": []any{"a", 7.0}, "side": "buy", "note": nil, "extra": "kept",
		"opts": map[string]any{"deep": "3"},
	}
	got, err := s.CheckInput(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["amount"] != 1.5 || got["count"] != 5.0 || got["live"] != true {
		t.Errorf("numbers/booleans: %v", got)
	}
	if tags := got["tags"].([]any); tags[1] != "7" {
		t.Errorf("list items are converted too: %v", tags)
	}
	if _, present := got["note"]; present {
		t.Error("a null optional input is dropped")
	}
	if got["extra"] != "kept" {
		t.Error("inputs the schema doesn't declare pass through")
	}
	if got["opts"].(map[string]any)["deep"] != 3.0 {
		t.Errorf("nested objects are checked: %v", got["opts"])
	}
	if in["amount"] != "1.5" {
		t.Error("the caller's input must not be modified")
	}
}

func TestCheckInput_ErrorsTellTheModelWhatToFix(t *testing.T) {
	s := obj([]string{"symbol", "amount"}, map[string]domain.Schema{
		"symbol": {Type: domain.SchemaTypeString},
		"amount": {Type: domain.SchemaTypeNumber},
		"count":  {Type: domain.SchemaTypeInteger},
		"live":   {Type: domain.SchemaTypeBoolean},
		"tags":   {Type: domain.SchemaTypeArray, Items: &domain.Schema{Type: domain.SchemaTypeInteger}},
		"side":   {Type: domain.SchemaTypeString, Enum: []string{"buy", "sell"}},
		"opts":   obj([]string{"deep"}, map[string]domain.Schema{"deep": {Type: domain.SchemaTypeInteger}}),
	})
	for name, tc := range map[string]struct {
		in   map[string]any
		want []string
	}{
		"missing required":    {map[string]any{"symbol": "X"}, []string{`"amount" is required`}},
		"empty required text": {map[string]any{"symbol": "", "amount": 1.0}, []string{`"symbol" is required`}},
		"null required":       {map[string]any{"symbol": "X", "amount": nil}, []string{`"amount" is required`}},
		"not a number":        {map[string]any{"symbol": "X", "amount": "lots"}, []string{`"amount" must be a number, got "lots"`}},
		"fraction for whole":  {map[string]any{"symbol": "X", "amount": 1.0, "count": 2.5}, []string{`"count" must be a whole number, got 2.5`}},
		"not a boolean":       {map[string]any{"symbol": "X", "amount": 1.0, "live": "maybe"}, []string{`"live" must be true or false`}},
		"wrong kind":          {map[string]any{"symbol": []any{"X"}, "amount": 1.0}, []string{`"symbol" must be text, got a list`}},
		"not a list":          {map[string]any{"symbol": "X", "amount": 1.0, "tags": "a"}, []string{`"tags" must be a list, got text`}},
		"bad list item":       {map[string]any{"symbol": "X", "amount": 1.0, "tags": []any{1.0, "x"}}, []string{`"tags" item 2 must be a number`}},
		"outside the enum":    {map[string]any{"symbol": "X", "amount": 1.0, "side": "hold"}, []string{`"side" must be one of buy, sell, got "hold"`}},
		"nested required":     {map[string]any{"symbol": "X", "amount": 1.0, "opts": map[string]any{}}, []string{`"opts" has a problem: "deep" is required`}},
		"object expected":     {map[string]any{"symbol": "X", "amount": 1.0, "opts": "x"}, []string{`"opts" must be an object, got text`}},
		"several at once":     {map[string]any{}, []string{`"amount" is required`, `"symbol" is required`}},
	} {
		_, err := s.CheckInput(tc.in)
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q should contain %q", name, err.Error(), w)
			}
		}
	}
}

func TestCheckInput_AcceptsEveryShapeJSONGives(t *testing.T) {
	s := obj(nil, map[string]domain.Schema{"n": {Type: domain.SchemaTypeInteger}, "l": {Type: domain.SchemaTypeArray}})
	var in map[string]any
	dec := json.NewDecoder(strings.NewReader(`{"n": 3, "l": [1, "a", {"k": 1}]}`))
	dec.UseNumber() // numbers as json.Number
	if err := dec.Decode(&in); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CheckInput(in); err != nil || got["n"] != 3.0 {
		t.Fatalf("json.Number: %v %v", got, err)
	}
	if _, err := s.CheckInput(map[string]any{"l": []string{"a", "b"}}); err != nil {
		t.Fatalf("typed slices from Go callers: %v", err)
	}
	// A schema with no inputs accepts an empty call, and a tool with no schema at all is untouched.
	if _, err := (domain.Schema{Type: domain.SchemaTypeObject}).CheckInput(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got, err := (domain.Schema{}).CheckInput(map[string]any{"anything": 1}); err != nil || got["anything"] != 1 {
		t.Fatalf("legacy tool without a schema: %v %v", got, err)
	}
}

type recordingExecutor struct {
	calls []map[string]any
}

func (r *recordingExecutor) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, in map[string]any) (any, error) {
	r.calls = append(r.calls, in)
	return "done", nil
}

type countingApprover struct{ asked int }

func (c *countingApprover) RequestApproval(context.Context, domain.ConversationRef, string, int) (bool, error) {
	c.asked++
	return true, nil
}

// The contract applies to every tool call, ahead of approval and ahead of the executor.
func TestHydratedTool_UsesItsInputContract(t *testing.T) {
	integration := &domain.Integration{ID: "svc", Service: "svc"}
	tool := &domain.ToolDefinition{
		ID: "svc.place_trade", Name: "place_trade", IntegrationID: "svc", Action: "place", Description: "Places a trade.",
		RequiresApproval: true, ApprovalPromptTemplate: "Trade {{amount}} of {{symbol}}?", ApprovalTimeoutSeconds: 30,
		InputSchema: obj([]string{"symbol", "amount"}, map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString}, "amount": {Type: domain.SchemaTypeNumber}}),
	}
	exec := &recordingExecutor{}
	reg := domain.NewExecutorRegistry()
	reg.Register("svc", exec)
	bound, err := domain.HydrateTool(tool, nil, integration, reg)
	if err != nil {
		t.Fatal(err)
	}
	approver := &countingApprover{}
	ctx := domain.WithApprovalRequester(domain.WithConversation(context.Background(), domain.ConversationRef{Provider: "x", ChatID: "c"}), approver)

	// A bad call is sent back to the model: nobody is asked, nothing runs.
	_, err = bound.Invoke(ctx, map[string]any{"symbol": "EURUSD", "amount": "lots"})
	if err == nil || !strings.Contains(err.Error(), `place_trade: invalid input: "amount" must be a number`) {
		t.Fatalf("error for the model: %v", err)
	}
	if approver.asked != 0 || len(exec.calls) != 0 {
		t.Fatalf("an invalid call must not reach approval or the executor: asked=%d calls=%d", approver.asked, len(exec.calls))
	}
	if _, err := bound.Invoke(ctx, map[string]any{"symbol": "EURUSD"}); err == nil || !strings.Contains(err.Error(), `"amount" is required`) {
		t.Fatalf("missing input: %v", err)
	}
	if approver.asked != 0 {
		t.Fatal("never ask someone to approve a call with a blank in it")
	}

	// A good call gets through with its inputs in the declared form.
	if _, err := bound.Invoke(ctx, map[string]any{"symbol": "EURUSD", "amount": "2.5"}); err != nil {
		t.Fatal(err)
	}
	if approver.asked != 1 || len(exec.calls) != 1 || exec.calls[0]["amount"] != 2.5 {
		t.Fatalf("a valid call: asked=%d calls=%v", approver.asked, exec.calls)
	}
}
