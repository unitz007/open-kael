package runtime

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unitz007/open-kael/domain"
)

const (
	// carryOverFor is how long the ability a conversation was just using stays with it,
	// so a short follow-up still belongs to it.
	carryOverFor = 30 * time.Minute
	// maxCarryOverRunes: only a short message is treated as a follow-up. A longer one
	// that matches nothing is a new subject, not a continuation.
	maxCarryOverRunes = 60
	carryOverMax      = 2000 // conversations remembered at once
)

// carryOver remembers, per conversation, which abilities the last message used. It is in memory; after a restart
// the conversation itself says what it last used (skillsLastUsed).
type carryOver struct {
	mu   sync.Mutex
	last map[string]carryEntry
	now  func() time.Time
}

type carryEntry struct {
	skills []string
	at     time.Time
}

func newCarryOver() *carryOver { return &carryOver{last: map[string]carryEntry{}, now: time.Now} }

// carryKey names a conversation: one person, one agent, one chat.
func carryKey(agentID string, conv domain.ConversationRef) string {
	if conv.UserID == "" {
		return ""
	}
	return agentID + "|" + conv.UserID + "|" + conv.IdentityID + "|" + conv.ChatID + "|" + conv.ThreadID
}

// remember records the abilities this conversation is using now.
func (c *carryOver) remember(key string, skills []string) {
	if key == "" || len(skills) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.last) >= carryOverMax {
		for k, e := range c.last {
			if now.Sub(e.at) > carryOverFor {
				delete(c.last, k)
			}
		}
		if len(c.last) >= carryOverMax { // still full: drop everything rather than grow
			c.last = map[string]carryEntry{}
		}
	}
	c.last[key] = carryEntry{skills: append([]string(nil), skills...), at: now}
}

// recall returns the abilities the conversation was using, if that was recent enough.
func (c *carryOver) recall(key string) []string {
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.last[key]
	if !ok || c.now().Sub(e.at) > carryOverFor {
		delete(c.last, key)
		return nil
	}
	return append([]string(nil), e.skills...)
}

// lastUsedTurns is how many of the person's recent messages are looked back over for what the conversation used.
const lastUsedTurns = 2

// skillsLastUsed is which of the agent's abilities the conversation's latest turns used, read from the conversation
// itself (which, unlike carryOver, outlives a restart): the abilities whose tools were called in the most recent of the
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
