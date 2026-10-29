package runtime

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

// Handoff hears a person's message before anything else answers it, once it is known who they are: a code carrying
// a chat over from somewhere else ("/start <code>" from a Telegram link, a short code typed in Slack), say. When it
// takes the message it answers it itself (say sends a message to the chat; ctx can ask the person a question where
// the chat app can) and nothing else sees it. It must not block for long.
type Handoff func(ctx context.Context, conv domain.ConversationRef, agentID, text string, say func(text string)) (taken bool)

// SetHandoff sets what hears each person's message first (see Handoff).
func (h *Host) SetHandoff(f Handoff) { h.handoff = f }

// tryHandoff offers a person's message to the handoff; true when it took it.
func (h *Host) tryHandoff(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) bool {
	if h.handoff == nil || msg.Conversation.UserID == "" || msg.Text == "" {
		return false
	}
	ctx = domain.WithConversation(ctx, msg.Conversation)
	if cr, ok := resolveChoiceRequester(hosted, msg.Conversation); ok {
		ctx = domain.WithChoiceRequester(ctx, cr)
	}
	say := func(text string) { h.deliverBestEffort(ctx, hosted, msg.Conversation, text) }
	return h.handoff(ctx, msg.Conversation, hosted.Agent.ID, msg.Text, say)
}
