package runtime

import (
	"context"
	"log"
	"strings"

	"github.com/unitz007/open-kael/domain"
)

// ── Handling inbound messages ─────────────────────────────────────────────────
//
// An agent used to handle every message it received one at a time, from one queue, across all
// its chats and people. One slow turn (a model that takes a minute, a question waiting for a
// tap, an approval nobody answers) held up everybody. Now each conversation is its own line:
// messages in the SAME conversation are still handled strictly in order (the history they build
// on needs that), but different conversations proceed side by side.

// convQueue is the messages waiting behind the one being handled in a conversation.
type convQueue struct {
	msgs []domain.InboundMessage
	// current is the message being handled and cancel stops its turn (an edit of it: domain.ErrEdited).
	current string
	cancel  context.CancelCauseFunc
}

// dispatchInbound takes one message off the agent's inbox and gets it handled, without ever
// waiting for a turn to finish: it returns as soon as the message is on its way.
func (h *Host) dispatchInbound(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	if msg.ThreadID != "" {
		msg.Conversation.ThreadID = msg.ThreadID
	}

	// A button tap or a form submission is not a turn and never waits in line: it may be the very
	// thing a waiting turn needs (an approval, a choice).
	if msg.CallbackQuery != nil || msg.InstructionsSubmission != nil {
		go h.handleInboundSafely(ctx, hosted, msg)
		return
	}
	// The person changed a message they had sent (editing.go).
	if msg.Edited {
		h.dispatchEdit(ctx, hosted, msg)
		return
	}

	// Words typed while a question is waiting for this person cut the question short: its turn
	// ends at once, and these words are handled next, as a message that follows the question.
	// (Without this the question would hold them up until it timed out.)
	h.supersedeWaitingChoice(hosted, msg)

	h.enqueueInbound(ctx, hosted, msg)
}

// supersedeWaitingChoice cuts short the question waiting in msg's conversation, if any.
func (h *Host) supersedeWaitingChoice(hosted *HostedAgent, msg domain.InboundMessage) {
	if strings.TrimSpace(msg.Text) == "" {
		return
	}
	_, integration := resolveIdentityAndIntegration(hosted, msg.Conversation)
	if integration == nil {
		return
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return
	}
	if s, ok := executor.(domain.ChoiceSuperseder); ok {
		s.SupersedeChoice(msg.Conversation)
	}
}

// enqueueInbound adds msg to its conversation's line. The first message of an idle conversation
// starts a goroutine that works through the line and ends when it is empty, so a conversation
// that has gone quiet holds nothing.
func (h *Host) enqueueInbound(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	key := hosted.Agent.ID + "|" + conversationKey(msg.Conversation)

	h.convMu.Lock()
	if q, busy := h.convQueues[key]; busy {
		q.msgs = append(q.msgs, msg)
		h.convMu.Unlock()
		return
	}
	q := &convQueue{}
	h.convQueues[key] = q
	h.convMu.Unlock()

	h.serveWG.Add(1)
	go func(msg domain.InboundMessage) {
		defer h.serveWG.Done()
		for {
			turnCtx, cancel := context.WithCancelCause(ctx)
			h.convMu.Lock()
			q.current, q.cancel = msg.MessageID, cancel
			h.convMu.Unlock()
			h.handleInboundSafely(turnCtx, hosted, msg)
			cancel(nil)

			h.convMu.Lock()
			q.current, q.cancel = "", nil
			if len(q.msgs) == 0 || ctx.Err() != nil {
				if n := len(q.msgs); n > 0 {
					log.Printf("runtime: agent %q: shutting down with %d message(s) still queued in %s", hosted.Agent.ID, n, key)
				}
				delete(h.convQueues, key)
				h.convMu.Unlock()
				return
			}
			msg = q.msgs[0]
			q.msgs = q.msgs[1:]
			h.convMu.Unlock()
		}
	}(msg)
}
