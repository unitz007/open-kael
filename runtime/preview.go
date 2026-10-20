package runtime

import (
	"context"
	"strings"

	"github.com/unitz007/open-kael/domain"
)

// ToolUse is what a model would do with one message, tried without running anything.
type ToolUse struct {
	// Uses are the plain names of the abilities it would call first, in order, each once.
	Uses []string
	// Changes is true when any of them changes something rather than only looking it up.
	Changes bool
}

// PreviewToolUse tries a capability without running it: it builds what the model would be shown for this message
// the way a real turn does (the agent's own prompt, then the creator's instructions for the capability, then the
// message), offers it the abilities of the capability's connections, and reports which it would call. Nothing is
// executed, so it is safe to run while someone is still typing their instructions. It reports the model's first
// move: the abilities it reaches for before it has seen any result.
func PreviewToolUse(ctx context.Context, llm domain.LLM, agent *domain.Agent, skill *domain.Skill, defs []*domain.ToolDefinition, userText string) (ToolUse, error) {
	messages := []domain.Message{{Role: domain.RoleSystem, Content: buildSystemPrompt(agent, "", "", nil, nil, nil, domain.UserDetails{})}}
	if hint := capabilityInstructionsHint([]*domain.Skill{skill}, []string{skill.Name}); hint != "" {
		messages = append(messages, domain.Message{Role: domain.RoleSystem, Content: hint})
	}
	messages = append(messages, domain.Message{Role: domain.RoleUser, Content: userText})

	byName := make(map[string]*domain.ToolDefinition, len(defs))
	specs := make([]domain.ActionSpec, 0, len(defs)+1)
	for _, d := range defs {
		if d.Disabled {
			continue
		}
		s := d.Spec()
		byName[s.Name] = d
		specs = append(specs, domain.ActionSpec{
			Name: s.Name, Description: s.Description, Instructions: s.Instructions, InputSchema: s.Input,
			ReadOnly: s.Effect == domain.EffectReads, Changes: s.Effect == domain.EffectChanges,
		})
	}
	specs = append(specs, buildFinishAction(false).Spec)

	resp, err := llm.Call(ctx, messages, specs)
	if err != nil {
		return ToolUse{}, err
	}
	var out ToolUse
	seen := map[string]bool{}
	for _, c := range resp.ToolCalls {
		d, ok := byName[c.Name]
		if !ok || seen[c.Name] {
			continue // the finish action, or something that is not on offer
		}
		seen[c.Name] = true
		out.Uses = append(out.Uses, plainAbilityName(d))
		if d.Spec().Effect == domain.EffectChanges {
			out.Changes = true
		}
	}
	return out, nil
}

// plainAbilityName is an ability's name in words a person would use: its title, as written for people.
func plainAbilityName(d *domain.ToolDefinition) string {
	name := strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(d.Name))
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return d.ModelName()
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
