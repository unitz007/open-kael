package runtime

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

// PlatformSkill is a capability the platform itself gives an agent (alerts, for one). It is not stored with the
// agent's own capabilities and is not its creator's to change. The router picks it by its description like any other,
// and when it is picked its actions are what the model is given. Its actions are also offered on every routed turn of
// a person, the way extra actions are, so a message that needs it beside another capability ("when a new signal comes
// in, analyse it") still has it. It is never offered on an unattended run.
type PlatformSkill struct {
	// Skill needs a Name (model-safe, unlike any name a creator could give: prefix it, e.g. platform_alerts), an ID
	// (counted as the capability's uses) and a Description the router matches against.
	Skill   *domain.Skill
	Actions []*domain.BoundAction
}

// SetPlatformSkills registers a function that supplies the platform's capabilities for a person's turn with an agent.
func (h *Host) SetPlatformSkills(f func(ctx context.Context, hosted *HostedAgent) []PlatformSkill) {
	h.platformSkills = f
}

type platformSkillsKey struct{}

func withPlatformSkills(ctx context.Context, ps []PlatformSkill) context.Context {
	if len(ps) == 0 {
		return ctx
	}
	return context.WithValue(ctx, platformSkillsKey{}, ps)
}

func platformSkillsFrom(ctx context.Context) []PlatformSkill {
	ps, _ := ctx.Value(platformSkillsKey{}).([]PlatformSkill)
	return ps
}

// skillsFor is every capability the router may pick on this turn: the agent's own, then the platform's.
func skillsFor(ctx context.Context, hosted *HostedAgent) []*domain.Skill {
	ps := platformSkillsFrom(ctx)
	if len(ps) == 0 {
		return hosted.Agent.Skills
	}
	out := make([]*domain.Skill, 0, len(hosted.Agent.Skills)+len(ps))
	out = append(out, hosted.Agent.Skills...)
	for _, p := range ps {
		out = append(out, p.Skill)
	}
	return out
}

// platformActions returns the actions of the platform capability called name, if it is one.
func platformActions(ctx context.Context, name string) ([]*domain.BoundAction, bool) {
	for _, p := range platformSkillsFrom(ctx) {
		if p.Skill.Name == name {
			return p.Actions, true
		}
	}
	return nil, false
}

// isPlatformAction says whether an action belongs to a platform capability. Such actions carry their own confirmation,
// so a question ("can you tell me when …") still reaches them where only look-ups are offered.
func isPlatformAction(ctx context.Context, name string) bool {
	for _, p := range platformSkillsFrom(ctx) {
		for _, a := range p.Actions {
			if a.Spec.Name == name {
				return true
			}
		}
	}
	return false
}
