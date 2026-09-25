package runtime

import (
	"context"
	"fmt"
	"log"

	"github.com/unitz007/open-kael/domain"
)

// runSkill is the shared headless execution path for all non-conversational
// skill triggers (cron, event). It differs from HandleTurn in that there is
// no user, no LLM loop, and no memory — just: hydrate tools, bind the skill,
// invoke, deliver the result to NotifyConversation if set.
//
// input carries any trigger-specific fields the skill's tools can read:
//   - cron:  nil (no payload)
//   - event: {trigger_input: "<event payload>"}
//
// notify_conversation is injected automatically when Trigger.NotifyConversation
// is set, so tools can post progress updates to that destination while running.
//
// Identity is resolved via the agent's IdentityIDs through HydrateSkillTools —
// the same path as conversational turns. No per-user connection refs are
// injected because triggered skills act as the bot identity, not on behalf of
// a specific user. Tools that need user context receive it through trigger_input.
func (h *Host) runSkill(ctx context.Context, hosted *HostedAgent, skill *domain.Skill, input map[string]any) {
	defer recoverFromPanic(hosted.Agent.ID, "triggered skill "+skill.Name)

	tools, err := domain.HydrateSkillTools(
		skill, hosted.Agent,
		hosted.Deps.ToolsByID,
		hosted.Deps.IdentitiesByID,
		hosted.Deps.IntegrationsByID,
		hosted.Deps.Executors,
	)
	if err != nil {
		log.Printf("runtime: agent %q: skill %q: hydrate: %v", hosted.Agent.ID, skill.Name, err)
		return
	}

	if input == nil {
		input = map[string]any{}
	}
	if skill.Trigger != nil && skill.Trigger.NotifyConversation != nil {
		input[notifyConversationInputKey] = *skill.Trigger.NotifyConversation
	}

	action := domain.BindSkill(hosted.Agent, skill, tools)

	log.Printf("runtime: agent %q: skill %q triggered", hosted.Agent.ID, skill.Name)
	output, err := action.Invoke(ctx, input)
	if err != nil {
		log.Printf("runtime: agent %q: skill %q failed: %v", hosted.Agent.ID, skill.Name, err)
		if skill.Trigger != nil && skill.Trigger.NotifyConversation != nil {
			h.deliverBestEffort(ctx, hosted, *skill.Trigger.NotifyConversation,
				fmt.Sprintf("%s failed: %v", skill.Name, err))
		}
		return
	}

	log.Printf("runtime: agent %q: skill %q finished: %v", hosted.Agent.ID, skill.Name, output)
	if skill.Trigger != nil && skill.Trigger.NotifyConversation != nil {
		h.deliverBestEffort(ctx, hosted, *skill.Trigger.NotifyConversation, stringifyOutput(output))
	}
}
