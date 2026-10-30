package runtime

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unitz007/open-kael/domain"
)

const (
	// maxCarryOverRunes: only a short message is treated as a follow-up. A longer one that matches nothing is a new
	// subject, not a continuation.
	maxCarryOverRunes = 60
	// followUpFor is how long what a conversation was just doing stays with it, so a short follow-up still belongs to it.
	followUpFor = 30 * time.Minute
)

// followUpKey names a conversation for its follow-ups: one agent, one chat (and thread), one person.
func followUpKey(ctx context.Context, hosted *HostedAgent) domain.ConversationKey {
	conv, _ := domain.ConversationFromContext(ctx)
	return domain.ConversationKey{AgentID: hosted.Agent.ID, IdentityID: conv.IdentityID, ChatID: conv.ChatID, ThreadID: conv.ThreadID, UserID: conv.UserID}
}

// lastUsedTurns is how many of the person's recent messages are looked back over for what the conversation used.
const lastUsedTurns = 2

// skillsLastUsed is which of the agent's abilities the conversation's latest turns used, read from the conversation
// itself: the abilities whose tools were called in the most recent of the
// last lastUsedTurns turns that called any. messages ends with the message being answered.
func (h *Host) skillsLastUsed(hosted *HostedAgent, messages []domain.Message) []string {
	used := map[string]bool{}
	turns := 0
	for i := len(messages) - 2; i >= 0 && turns < lastUsedTurns; i-- {
		m := messages[i]
		switch m.Role {
		case domain.RoleUser:
			if len(used) > 0 {
				i = -1 // that turn used something: it is the one
				continue
			}
			turns++
		case domain.RoleAssistant:
			for _, c := range m.ToolCalls {
				if c.Name != domain.FinishActionName {
					used[c.Name] = true
				}
			}
		}
	}
	if len(used) == 0 {
		return nil
	}
	var out []string
	for _, skill := range hosted.Agent.Skills {
		tools, err := h.hydrate(hosted, skill)
		if err != nil {
			continue
		}
		for _, t := range tools {
			if used[t.Spec.Name] {
				out = append(out, skill.Name)
				break
			}
		}
	}
	return out
}

// isShortFollowUp reports whether a message is short enough to be a continuation.
func isShortFollowUp(text string) bool {
	text = strings.TrimSpace(text)
	return text != "" && utf8.RuneCountInString(text) <= maxCarryOverRunes
}

// declineText is what a person is told when what they asked is outside everything the
// agent does. It is fixed wording built from the agent's own list of abilities, so the
// agent can never promise work it isn't going to do.
func declineText(abilities []string) string {
	var b strings.Builder
	b.WriteString("I can't help with that one.")
	if len(abilities) > 0 {
		b.WriteString(" Here's what I can do:")
		for _, a := range abilities {
			b.WriteString("\n• " + a)
		}
		b.WriteString("\n\nWhat would you like to try?")
	}
	return b.String()
}

// fixedReply ends a turn with a reply that no model wrote: it is delivered and kept in the
// conversation like any other.
func fixedReply(messages []domain.Message, text string) (*domain.LoopResult, []domain.Message) {
	out := append(append([]domain.Message(nil), messages...), domain.Message{Role: domain.RoleAssistant, Content: text})
	return &domain.LoopResult{Status: domain.LoopStatusComplete, Content: text}, out
}
