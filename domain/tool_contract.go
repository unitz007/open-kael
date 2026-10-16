package domain

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The tool contract.
//
// An agent must not be able to tell where a tool came from. A built-in tool
// written in code, one discovered from an OpenAPI file and one served by a tool
// server are all just a ToolDefinition, and every one of them must be complete
// when it is registered:
//
//   - a stable ID and the integration it belongs to;
//   - the name the model calls it by: letters, digits, "_" and "-", starting with
//     a letter, at most 64 characters (FunctionName, or Name when that is empty);
//   - a plain-words Description, so the model knows when to use it;
//   - an Action, the key its Executor dispatches on;
//   - an object InputSchema whose required inputs all exist, each with a type;
//   - an effect, decided up front: ReadOnly means it only retrieves information and
//     is the only kind of tool offered when someone merely asks for advice. Anything
//     else is a change. There is no "unknown": a source that can't tell must say
//     "changes" until a person says otherwise;
//   - approval settings that make sense: a timeout that isn't negative, and an
//     approval prompt that only mentions inputs the tool takes.
//
// Sources register tools through ValidateTool (or ValidateTools) so a tool that
// breaks the contract is refused where it enters, not discovered when an agent
// misbehaves. ToolContract below is the same text as data, for tests and docs.

const (
	// MaxToolNameLen is the longest name a model will accept for a tool.
	MaxToolNameLen = 64
	// MaxToolDescriptionLen keeps one tool from flooding the model's context.
	MaxToolDescriptionLen = 1024
)

var toolNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// Effect says what running a tool does to the outside world.
type Effect string

const (
	// EffectReads: only retrieves information.
	EffectReads Effect = "reads"
	// EffectChanges: anything else — creates, changes, sends, deletes, spends.
	EffectChanges Effect = "changes"
)

// Effect is the tool's effect as the runtime sees it.
func (t *ToolDefinition) Effect() Effect {
	if t.ReadOnly {
		return EffectReads
	}
	return EffectChanges
}

// ModelName is the name the model calls the tool by.
func (t *ToolDefinition) ModelName() string {
	if t.FunctionName != "" {
		return t.FunctionName
	}
	return t.Name
}

// ToolProblem is one way a tool breaks the contract.
type ToolProblem struct {
	Tool    string // the tool's ID
	Message string
}

func (p ToolProblem) Error() string { return fmt.Sprintf("tool %q: %s", p.Tool, p.Message) }

var promptFieldRe = regexp.MustCompile(`\{\{\s*([^{}\s]+)\s*\}\}`)

// ValidateTool reports every way t breaks the contract (nil when it keeps it).
func ValidateTool(t *ToolDefinition) error {
	if t == nil {
		return errors.New("tool is nil")
	}
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, ToolProblem{Tool: t.ID, Message: fmt.Sprintf(format, a...)})
	}

	if strings.TrimSpace(t.ID) == "" {
		bad("has no ID")
	}
	if strings.TrimSpace(t.IntegrationID) == "" {
		bad("belongs to no integration")
	}
	if name := t.ModelName(); !toolNameRe.MatchString(name) {
		bad("the name the model sees, %q, must start with a letter and use only letters, digits, _ and - (at most %d characters)", name, MaxToolNameLen)
	}
	switch d := strings.TrimSpace(t.Description); {
	case d == "":
		bad("has no description, so the model can't tell when to use it")
	case len(d) > MaxToolDescriptionLen:
		bad("description is %d characters; the most allowed is %d", len(d), MaxToolDescriptionLen)
	}
	if strings.TrimSpace(t.Action) == "" {
		bad("has no action for its executor")
	}

	if t.InputSchema.Type != SchemaTypeObject {
		bad("input schema must be an object, not %q", t.InputSchema.Type)
	}
	props := t.InputSchema.Properties
	for _, r := range t.InputSchema.Required {
		if _, ok := props[r]; !ok {
			bad("requires input %q, which it doesn't declare", r)
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if props[n].Type == "" {
			bad("input %q has no type", n)
		}
	}

	if t.ApprovalTimeoutSeconds < 0 {
		bad("approval timeout can't be negative")
	}
	if t.MaxCalls < 0 {
		bad("max calls can't be negative")
	}
	if t.RequiresApproval {
		for _, m := range promptFieldRe.FindAllStringSubmatch(t.ApprovalPromptTemplate, -1) {
			if _, ok := props[m[1]]; !ok {
				bad("approval prompt mentions {{%s}}, which the tool doesn't take", m[1])
			}
		}
	}
	return errors.Join(errs...)
}

// ValidateTools checks a source's tools together: each must keep the contract and
// no two may share a model name (the model couldn't tell them apart).
func ValidateTools(tools []*ToolDefinition) error {
	var errs []error
	byID := map[string]bool{}
	byName := map[string]string{}
	for _, t := range tools {
		if err := ValidateTool(t); err != nil {
			errs = append(errs, err)
		}
		if t == nil {
			continue
		}
		if byID[t.ID] {
			errs = append(errs, ToolProblem{Tool: t.ID, Message: "ID is used by more than one tool"})
		}
		byID[t.ID] = true
		if other, dup := byName[t.IntegrationID+"/"+t.ModelName()]; dup {
			errs = append(errs, ToolProblem{Tool: t.ID, Message: fmt.Sprintf("has the same name as %q within its integration", other)})
		}
		byName[t.IntegrationID+"/"+t.ModelName()] = t.ID
	}
	return errors.Join(errs...)
}
