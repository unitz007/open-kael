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
	log.Printf("skill-router: execute intent — any one of these succeeding means the request is done: %v", names)
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

// lastAssistant returns the agent's most recent reply in full (empty if none) and where
// it sits in messages. A reply is stored either as plain content or, for an agent that
// finishes with a tool call, as that call's "content" argument; both are read.
func lastAssistant(messages []domain.Message) (text string, at int) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != domain.RoleAssistant {
			continue
		}
		t := strings.TrimSpace(messages[i].Content)
		if t == "" {
			for _, call := range messages[i].ToolCalls {
				if call.Name == domain.FinishActionName {
					if c, ok := call.Arguments["content"].(string); ok {
						t = strings.TrimSpace(c)
					}
				}
			}
		}
		if t != "" {
			return t, i
		}
	}
	return "", -1
}

// lastAssistantText is the agent's most recent reply, cut to its first maxEarlierRunes
// characters: what a short follow-up like "yes" is judged against.
func lastAssistantText(messages []domain.Message) string {
	text, _ := lastAssistant(messages)
	if r := []rune(text); len(r) > maxEarlierRunes {
		text = string(r[:maxEarlierRunes]) + "…"
	}
	return text
}

// recentTurn is what was said just before the latest message: the agent's last reply and
// the person's message that it answered.
func recentTurn(messages []domain.Message) domain.Earlier {
	reply, at := lastAssistant(messages)
	if at < 0 {
		return domain.Earlier{}
	}
	e := domain.Earlier{Assistant: reply}
	for i := at - 1; i >= 0; i-- {
		if messages[i].Role == domain.RoleUser {
			if t := strings.TrimSpace(messages[i].Content); t != "" {
				e.User = t
				break
			}
		}
	}
	return e
}

// keepNeeded puts back what an offered action must run first (BoundAction.Needs) when trimming left it out: without it
// the action could never run.
func keepNeeded(offered, all []*domain.BoundAction) []*domain.BoundAction {
	have := map[string]bool{}
	for _, t := range offered {
		have[t.Spec.Name] = true
	}
	for i := 0; i < len(offered); i++ { // what is put back may need something too
		for _, need := range offered[i].Needs {
			if have[need] {
				continue
			}
			for _, t := range all {
				if t.Spec.Name == need {
					offered = append(offered, t)
					have[need] = true
					break
				}
			}
		}
	}
	return offered
}
