package runtime

import (
	"context"
	"fmt"

	"github.com/unitz007/open-kael/domain"
)

const maxQuerySkillDepth = 3

type ctxQueryDepth struct{}

func querySkillDepth(ctx context.Context) int {
	d, _ := ctx.Value(ctxQueryDepth{}).(int)
	return d
}

func withQuerySkillDepth(ctx context.Context, d int) context.Context {
	return context.WithValue(ctx, ctxQueryDepth{}, d)
}

// runSkillQuery runs a skill headlessly and returns its output as a string.
// The result is captured so querySkillAction can return it to the calling skill's LLM mid-turn.
func (h *Host) runSkillQuery(ctx context.Context, hosted *HostedAgent, skill *domain.Skill, input map[string]any) (string, error) {
	tools, err := h.hydrate(hosted, skill)
	if err != nil {
		return "", fmt.Errorf("query_skill: hydrate %q: %w", skill.Name, err)
	}
	action := domain.BindSkill(hosted.Agent, skill, tools)
	output, err := action.Invoke(ctx, input)
	if err != nil {
		return "", fmt.Errorf("query_skill: invoke %q: %w", skill.Name, err)
	}
	return domain.StringifyResult(output), nil
}
