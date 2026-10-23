package runtime

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

// When routing picked a capability but what the person asked for is beyond what it can do ("move my stop to
// break-even" to a trading agent that can only place trades), the agent says it can't. Until 2026-10-23 nothing
// counted that, so the creator never saw it under "asked for, but not possible yet": only messages that matched no
// capability at all were counted. The agent now states it on finish, and the request is counted like those.

// noAbilityField is the finish input in which the agent says the request was beyond all its abilities.
const noAbilityField = "no_ability_for_it"

type askAboutAbilityKey struct{}

// withAbilityQuestion marks a turn on which a capability was picked, so finish asks whether the request was beyond it.
func withAbilityQuestion(ctx context.Context) context.Context {
	return context.WithValue(ctx, askAboutAbilityKey{}, true)
}

// asksAboutAbility: only a person's own turn on which a capability was picked; never an unattended run (an alert or a
// repeating task is not someone asking for something).
func asksAboutAbility(ctx context.Context) bool {
	on, _ := ctx.Value(askAboutAbilityKey{}).(bool)
	return on && !domain.IsUnattended(ctx)
}

// addAbilityQuestionTo lets finish say the request was beyond every ability the agent has.
func addAbilityQuestionTo(a *domain.BoundAction) {
	a.Spec.InputSchema.Properties[noAbilityField] = domain.Schema{
		Type: domain.SchemaTypeBoolean,
		Description: "true only when what the person asked for is something none of your abilities can do at all, so you are " +
			"telling them you can't (for example they asked you to change or look up something and no action you have does " +
			"that). false when you did it or answered it, when you need a detail from them first, when their account isn't " +
			"connected, when they said no, or when something you tried failed.",
	}
	a.Spec.InputSchema.Required = append(a.Spec.InputSchema.Required, noAbilityField)
}

// beyondAbilities reports whether the agent finished saying the request was beyond all its abilities.
func beyondAbilities(result *domain.LoopResult) bool {
	if result == nil || result.Output == nil {
		return false
	}
	switch v := result.Output[noAbilityField].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}
