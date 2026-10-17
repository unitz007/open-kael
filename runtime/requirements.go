package runtime

import (
	"context"
	"log"
	"strings"

	"github.com/unitz007/open-kael/domain"
)

// ToolRequirer works out, from what a person asked, which of the tools that change
// something must really run for the request to be done. It is the one question that
// decides what "done" means for a turn; the loop then holds the agent to it (see
// domain.Requirements). Implemented by the Jev client.
//
// userText is what the person just wrote. earlier is the assistant's previous message
// in the conversation (empty if none), because a short reply such as "yes" or "do it"
// only means something next to what it answers. tools are only the ones that change
// something. It returns the names of those the request needs (possibly none, for a
// request that needs no action).
type ToolRequirer interface {
	RequiredTools(ctx context.Context, userText, earlier string, tools []*domain.BoundAction) ([]string, error)
}

// SetToolRequirer turns on holding agents to what they claim. Without one, turns
// run as they always did.
func (h *Host) SetToolRequirer(r ToolRequirer) { h.toolRequirer = r }

// requirementsFor decides what must have really happened before this turn may be
// called done. Only a request to DO something (execute intent) is held to anything,
// and only when an action that changes something is in play.
func (h *Host) requirementsFor(ctx context.Context, userText string, earlier string, intent domain.Intent, tools []*domain.BoundAction) *domain.Requirements {
	if h.toolRequirer == nil || intent != domain.IntentExecute {
		return nil
	}
	var changing []*domain.BoundAction
	for _, t := range tools {
		if t.Spec.Changes {
			changing = append(changing, t)
		}
	}
	if len(changing) == 0 {
		return nil
	}
	names, err := h.toolRequirer.RequiredTools(ctx, userText, earlier, changing)
	if err != nil {
		// Can't tell which one the request needs, so insist that some real change
		// happened rather than let a claim through unchecked.
		log.Printf("skill-router: could not work out the required action: %v — requiring any change", err)
		return &domain.Requirements{AnyChange: true}
	}
	if len(names) == 0 {
		return nil
	}
	log.Printf("skill-router: execute intent — these must succeed for the request to be done: %v", names)
	return &domain.Requirements{Tools: names}
}

// keepRequired puts back any required action the relevance filter trimmed away: an
// agent can't be held to doing something it was never offered.
func keepRequired(offered, all []*domain.BoundAction, reqs *domain.Requirements) []*domain.BoundAction {
	if reqs == nil || len(reqs.Tools) == 0 {
		return offered
	}
	have := map[string]bool{}
	for _, t := range offered {
		have[t.Spec.Name] = true
	}
	for _, name := range reqs.Tools {
		if have[name] {
			continue
		}
		for _, t := range all {
			if t.Spec.Name == name {
				offered = append(offered, t)
				have[name] = true
				break
			}
		}
	}
	return offered
}

func actionNames(actions []*domain.BoundAction) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = a.Spec.Name
	}
	return out
}

// maxEarlierRunes bounds how much of the previous message is shown to Jev.
const maxEarlierRunes = 600

// lastAssistantText is the assistant's most recent message with any words in it.
// An agent speaks by calling finish, so in the stored history its words are the
// content of that call, not the message's own text.
func lastAssistantText(messages []domain.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != domain.RoleAssistant {
			continue
		}
		text := strings.TrimSpace(messages[i].Content)
		if text == "" {
			for _, call := range messages[i].ToolCalls {
				if call.Name == domain.FinishActionName {
					if c, ok := call.Arguments["content"].(string); ok {
						text = strings.TrimSpace(c)
					}
				}
			}
		}
		if text == "" {
			continue
		}
		if r := []rune(text); len(r) > maxEarlierRunes {
			text = string(r[:maxEarlierRunes]) + "…"
		}
		return text
	}
	return ""
}
