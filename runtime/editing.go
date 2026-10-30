package runtime

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// ── A person editing a message they sent (domain/editing.go) ──────────────────

// editedHint tells the agent about an edit whose old reply already changed something.
const editedHint = `The person edited their last message: it said "%s" and now says "%s". Your reply to the old words already did this: %s. Don't do it again and don't undo it on your own. Say plainly what was already done (and that it can be undone if they ask), then help with the new words; anything that would change something, offer it as a choice and wait for their yes.`

// noteExchange keeps the person's message and the reply to it with the conversation, so an edit of the message can
// be answered (only where the conversation memory keeps exchanges, and the chat app said which message it was).
func (h *Host) noteExchange(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, key domain.ConversationKey, text, replyID string) {
	ex, ok := hosted.Deps.Memory.(domain.Exchanges)
	if !ok || domain.IsUnattended(ctx) || conv.UserID == "" || conv.MessageID == "" {
		return
	}
	var changed []string
	for _, s := range turnStepsOf(ctx, hosted) {
		if s.Changes && s.State == domain.StepDone {
			changed = append(changed, s.Name)
		}
	}
	ex.SetLastExchange(ctx, key, domain.LastExchange{MessageID: conv.MessageID, Text: text, At: time.Now(), ReplyID: replyID, Changed: changed})
}

// dispatchEdit decides what an edited message means: waiting in line, its new words take its place; being answered,
// that reply is dropped and the new words answered next; answered already, see answerEdit.
func (h *Host) dispatchEdit(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	if strings.TrimSpace(msg.Text) == "" || msg.MessageID == "" {
		return
	}
	fresh := msg
	fresh.Edited = false
	// Answered already (its reply is out, even if what follows the reply is still running): see answerEdit.
	if h.answered(ctx, hosted, msg) {
		go h.answerEdit(ctx, hosted, fresh)
		return
	}
	key := hosted.Agent.ID + "|" + conversationKey(msg.Conversation)
	h.convMu.Lock()
	if q, busy := h.convQueues[key]; busy {
		for i := range q.msgs {
			if q.msgs[i].MessageID == msg.MessageID {
				q.msgs[i].Text = msg.Text
				h.convMu.Unlock()
				return
			}
		}
		if q.current == msg.MessageID && q.cancel != nil {
			q.cancel(domain.ErrEdited)
			q.msgs = append([]domain.InboundMessage{fresh}, q.msgs...)
			h.convMu.Unlock()
			log.Printf("runtime: agent %q: a message being answered was edited: answering its new words instead", hosted.Agent.ID)
			return
		}
	}
	h.convMu.Unlock()
	go h.answerEdit(ctx, hosted, fresh)
}

// answered says whether a message's reply has gone out (it is the conversation's last exchange).
func (h *Host) answered(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) bool {
	ex, ok := hosted.Deps.Memory.(domain.Exchanges)
	if !ok {
		return false
	}
	conv := h.withPerson(ctx, msg)
	last, ok := ex.LastExchange(ctx, domain.ConversationKey{AgentID: hosted.Agent.ID, IdentityID: conv.IdentityID, ChatID: conv.ChatID, ThreadID: conv.ThreadID, UserID: conv.UserID})
	return ok && last.MessageID == msg.MessageID
}

// withPerson is a message's conversation with who wrote it.
func (h *Host) withPerson(ctx context.Context, msg domain.InboundMessage) domain.ConversationRef {
	conv := msg.Conversation
	if conv.UserID == "" && h.userChannelResolver != nil {
		if id, err := h.userChannelResolver(ctx, conv.IdentityID, conv.ChatID, conv.SenderID); err == nil {
			conv.UserID = id
		}
	}
	if msg.ThreadID != "" {
		conv.ThreadID = msg.ThreadID
	}
	return conv
}

// ignoredEditNote is what a person is told when they change a message the agent no longer goes by.
const ignoredEditNote = "I only take in a change to your latest message, for 15 minutes after you send it. Write it again as a new message if you'd like me to look at it."

// noteIgnoredEdit tells the person, at most once a day in a conversation, that their edit was left alone: chat apps
// such as Telegram let anyone change any old message, and a silent edit reads as one that was seen.
func (h *Host) noteIgnoredEdit(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, key domain.ConversationKey) {
	notes, ok := hosted.Deps.Memory.(domain.EditNotes)
	if !ok || !notes.MayNoteIgnoredEdit(ctx, key) {
		return
	}
	h.deliverBestEffort(ctx, hosted, conv, ignoredEditNote)
}

// answerEdit answers an edit of the person's latest message within domain.EditWindow: in the old reply's place when it
// only looked things up, else with look-ups only and told what was already done. An older message's edit changes
// nothing, and the person is told so (noteIgnoredEdit).
func (h *Host) answerEdit(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	ex, ok := hosted.Deps.Memory.(domain.Exchanges)
	if !ok {
		return
	}
	conv := h.withPerson(ctx, msg)
	key := domain.ConversationKey{AgentID: hosted.Agent.ID, IdentityID: conv.IdentityID, ChatID: conv.ChatID, ThreadID: conv.ThreadID, UserID: conv.UserID}
	last, ok := ex.LastExchange(ctx, key)
	if !ok {
		return
	}
	if last.MessageID != msg.MessageID || time.Since(last.At) > domain.EditWindow {
		h.noteIgnoredEdit(ctx, hosted, conv, key)
		return
	}
	if strings.TrimSpace(last.Text) == strings.TrimSpace(msg.Text) {
		return
	}
	msg.Conversation = conv
	if len(last.Changed) == 0 {
		if err := ex.RewindLastExchange(ctx, key); err != nil {
			log.Printf("runtime: agent %q: an edit: %v", hosted.Agent.ID, err)
			return
		}
		msg.ReplacesReply = last.ReplyID
	} else {
		msg.LookUpsOnly = true
		msg.Hint = fmt.Sprintf(editedHint, last.Text, msg.Text, strings.Join(last.Changed, ", "))
	}
	h.enqueueInbound(ctx, hosted, msg)
}
