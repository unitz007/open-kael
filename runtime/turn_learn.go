package runtime

import (
	"context"
	"log"
	"sort"

	"github.com/unitz007/open-kael/domain"
)

// Learning how a person wants the job done: after each exchange a person has with an agent, the learners added here
// hear it (the reply the person was answering, what they said, the reply they got and the apps it used), in the
// background, as details are learned. What they keep is told back to the agent before each reply by a prompt note.

// TurnLearn is one exchange, as a learner sees it.
type TurnLearn struct {
	Conv    domain.ConversationRef
	AgentID string
	// PrevReply is the agent's reply the person was answering ("" when there was none).
	PrevReply   string
	UserMessage string
	Reply       string
	// Apps is the apps this agent works with (id → name); UsedApps the ones this reply used, by id.
	Apps     map[string]string
	UsedApps []string
}

// TurnLearning hears each exchange a person has with an agent (never unattended runs). It runs in the background and
// must not block for long; the conversation's choice requester is in ctx when the chat app can ask.
type TurnLearning interface {
	LearnTurn(ctx context.Context, t TurnLearn) error
}

// AddTurnLearner adds a learner that hears every exchange.
func (h *Host) AddTurnLearner(l TurnLearning) { h.turnLearners = append(h.turnLearners, l) }

// AddPromptNote adds text to an agent's instructions for a person's message (what it learned with them, say), after
// the platform's own; "" adds nothing.
func (h *Host) AddPromptNote(f func(ctx context.Context, userID, agentID string) string) {
	h.promptNotes = append(h.promptNotes, f)
}

// afterTurn is what follows a reply to a person: details are learned, and every turn learner hears it.
func (h *Host) afterTurn(ctx context.Context, conv domain.ConversationRef, hosted *HostedAgent, prevReply, userText, reply string) {
	h.maybeLearn(ctx, conv, hosted, userText, reply)
	if conv.UserID == "" || len(h.turnLearners) == 0 {
		return
	}
	apps := map[string]string{}
	for id, i := range hosted.usedIntegrations() {
		apps[id] = i.Name
	}
	t := TurnLearn{Conv: conv, AgentID: hosted.Agent.ID, PrevReply: prevReply, UserMessage: userText, Reply: reply, Apps: apps, UsedApps: usedAppsOf(ctx, hosted)}
	bg := domain.WithConversation(context.Background(), conv)
	if cr, ok := domain.ChoiceRequesterFromContext(ctx); ok {
		bg = domain.WithChoiceRequester(bg, cr)
	}
	for _, l := range h.turnLearners {
		go func(l TurnLearning) {
			if err := l.LearnTurn(bg, t); err != nil {
				log.Printf("runtime: turn learner: agent %s: %v", hosted.Agent.ID, err)
			}
		}(l)
	}
}

// usedAppsOf is the apps (integration ids) of the abilities this piece of work used.
func usedAppsOf(ctx context.Context, hosted *HostedAgent) []string {
	t, ok := stepsOf(ctx)
	if !ok {
		return nil
	}
	tools, _, _ := hosted.View()
	seen := map[string]bool{}
	for _, s := range t.snapshot().Steps {
		if def, ok := tools[s.Tool]; ok && def.IntegrationID != "" {
			seen[def.IntegrationID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
