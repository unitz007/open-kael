package domain_test

import (
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func goodTool() *domain.ToolDefinition {
	return &domain.ToolDefinition{
		ID: "svc:get_quote", Name: "get_quote", IntegrationID: "svc", Action: "svc.get_quote",
		Description: "Shows the price for a symbol.", ReadOnly: true,
		InputSchema: domain.Schema{Type: domain.SchemaTypeObject, Required: []string{"symbol"},
			Properties: map[string]domain.Schema{"symbol": {Type: domain.SchemaTypeString}}},
	}
}

func TestValidateTool_AcceptsACompleteTool(t *testing.T) {
	if err := domain.ValidateTool(goodTool()); err != nil {
		t.Fatalf("a complete tool must pass: %v", err)
	}
	// A tool with no inputs is fine, and so is one named by FunctionName alone.
	tool := goodTool()
	tool.Name, tool.FunctionName = "Get quote", "get_quote"
	tool.InputSchema = domain.Schema{Type: domain.SchemaTypeObject}
	if err := domain.ValidateTool(tool); err != nil {
		t.Fatalf("no-input tool: %v", err)
	}
}

func TestValidateTool_NamesEveryWayItIsIncomplete(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*domain.ToolDefinition)
		want   string
	}{
		"no id":               {func(d *domain.ToolDefinition) { d.ID = "" }, "no ID"},
		"no integration":      {func(d *domain.ToolDefinition) { d.IntegrationID = " " }, "no integration"},
		"no name":             {func(d *domain.ToolDefinition) { d.Name, d.FunctionName = "", "" }, "name the model sees"},
		"name with colon":     {func(d *domain.ToolDefinition) { d.Name = "svc:get_quote" }, "name the model sees"},
		"name with a space":   {func(d *domain.ToolDefinition) { d.Name = "get quote" }, "name the model sees"},
		"name starts a digit": {func(d *domain.ToolDefinition) { d.Name = "1get" }, "name the model sees"},
		"name too long":       {func(d *domain.ToolDefinition) { d.Name = "a" + strings.Repeat("b", 64) }, "name the model sees"},
		"no description":      {func(d *domain.ToolDefinition) { d.Description = "  " }, "no description"},
		"huge description":    {func(d *domain.ToolDefinition) { d.Description = strings.Repeat("x", 1025) }, "most allowed"},
		"no action":           {func(d *domain.ToolDefinition) { d.Action = "" }, "no action"},
		"schema not object":   {func(d *domain.ToolDefinition) { d.InputSchema.Type = domain.SchemaTypeString }, "must be an object"},
		"schema type unset":   {func(d *domain.ToolDefinition) { d.InputSchema = domain.Schema{} }, "must be an object"},
		"requires unknown":    {func(d *domain.ToolDefinition) { d.InputSchema.Required = []string{"ghost"} }, `requires input "ghost"`},
		"input without type":  {func(d *domain.ToolDefinition) { d.InputSchema.Properties["symbol"] = domain.Schema{} }, `input "symbol" has no type`},
		"negative timeout":    {func(d *domain.ToolDefinition) { d.ApprovalTimeoutSeconds = -1 }, "timeout"},
		"negative max calls":  {func(d *domain.ToolDefinition) { d.MaxCalls = -1 }, "max calls"},
		"prompt mentions an input it lacks": {func(d *domain.ToolDefinition) {
			d.RequiresApproval, d.ApprovalPromptTemplate = true, "Buy {{symbol}} x {{amount}}?"
		}, "{{amount}}"},
	} {
		tool := goodTool()
		tc.mutate(tool)
		err := domain.ValidateTool(tool)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, tc.want, err)
		}
	}

	// All the problems are reported together, not one at a time.
	tool := goodTool()
	tool.ID, tool.Description, tool.Action = "", "", ""
	err := domain.ValidateTool(tool)
	for _, want := range []string{"no ID", "no description", "no action"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("combined error should mention %q: %v", want, err)
		}
	}
	if domain.ValidateTool(nil) == nil {
		t.Error("nil tool")
	}
}

func TestValidateTool_ApprovalPromptOnlyMattersWhenApprovalIsOn(t *testing.T) {
	tool := goodTool()
	tool.ApprovalPromptTemplate = "Buy {{amount}}?" // unused: approval is off
	if err := domain.ValidateTool(tool); err != nil {
		t.Fatalf("a stale prompt on a tool that doesn't ask is harmless: %v", err)
	}
	tool.RequiresApproval, tool.ApprovalPromptTemplate = true, "Look up {{symbol}}?"
	if err := domain.ValidateTool(tool); err != nil {
		t.Fatalf("a prompt that only uses real inputs: %v", err)
	}
}

func TestToolEffect_IsDecidedByReadOnly(t *testing.T) {
	r := goodTool()
	if r.Effect() != domain.EffectReads {
		t.Fatal("read-only tools read")
	}
	r.ReadOnly = false
	if r.Effect() != domain.EffectChanges {
		t.Fatal("anything not marked read-only is a change: there is no 'unknown'")
	}
}

func TestValidateTools_CatchesDuplicates(t *testing.T) {
	a, b := goodTool(), goodTool()
	b.ID = "svc:other" // same model name, same integration
	err := domain.ValidateTools([]*domain.ToolDefinition{a, b})
	if err == nil || !strings.Contains(err.Error(), "same name") {
		t.Fatalf("two tools the model can't tell apart: %v", err)
	}
	c := goodTool()
	c.IntegrationID = "elsewhere" // same name in a different integration is fine
	c.ID = "elsewhere:get_quote"
	if err := domain.ValidateTools([]*domain.ToolDefinition{a, c}); err != nil {
		t.Fatalf("same name in another integration: %v", err)
	}
	dup := goodTool()
	if err := domain.ValidateTools([]*domain.ToolDefinition{a, dup}); err == nil || !strings.Contains(err.Error(), "more than one tool") {
		t.Fatalf("duplicate id: %v", err)
	}
}
