package runtime

// Replies that carry choices. When the next step is a decision, the agent finishes with a few specific
// choices, shown as buttons under its message, so the person taps instead of typing. They are saved, so a
// tap works later and after a restart (domain.Proposal). A tap is handled exactly as if the person had
// typed the sentence behind the button, so it can never do anything typing could not.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// SetProposalStore turns on saved choices: without it, an agent's choices are shown as a plain list.
func (h *Host) SetProposalStore(s domain.ProposalStore) { h.proposals = s }

// canOfferChoices reports whether this turn's reply can carry tappable buttons: someone is there to tap,
// we know who they are, the choices can be saved, and the messenger can put buttons under a message.
func (h *Host) canOfferChoices(ctx context.Context, hosted *HostedAgent) bool {
	// Nobody is there to tap on an unattended run, except one that answers an alert: that one exists to reach them.
	if h.proposals == nil || (domain.IsUnattended(ctx) && !domain.IsAlertRun(ctx)) {
		return false
	}
	conv, ok := domain.ConversationFromContext(ctx)
	if !ok || conv.UserID == "" {
		return false
	}
	executor, _, _ := h.resolveExecutorForConv(ctx, hosted, conv)
	_, can := executor.(domain.ButtonAttacher)
	return can
}

// finishFor is the action the agent finishes with: with a way to offer choices where they can be tapped.
func (h *Host) finishFor(ctx context.Context, hosted *HostedAgent, withOutcome bool) *domain.BoundAction {
	a := buildFinishAction(withOutcome)
	if h.canOfferChoices(ctx, hosted) {
		addChoicesTo(a)
	}
	return a
}

// choicesSchema describes the choices an agent may finish with.
func choicesSchema() domain.Schema {
	return domain.Schema{
		Type: domain.SchemaTypeArray,
		Description: "Optional. When the best next step is a decision for the person, and you would otherwise end by asking " +
			"\"want me to…?\", give 2 to 5 ready next steps instead, shown as buttons so they can tap rather than type. " +
			"Do any look-ups first so each choice is specific (for example a real free time, not \"help with that\"). " +
			"Do not offer a choice for something you can simply do now because it only looks something up. " +
			"Put the question or situation in content, not in the buttons.",
		Items: &domain.Schema{
			Type: domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{
				"label": {Type: domain.SchemaTypeString, Description: "What the button says: a few words."},
				"say":   {Type: domain.SchemaTypeString, Description: "What the person is taken to say when they tap it, in the first person and specific, for example: Move my dentist appointment to 10:15 tomorrow."},
			},
			Required: []string{"label", "say"},
		},
	}
}

// addChoicesTo lets the finish action carry choices.
func addChoicesTo(a *domain.BoundAction) {
	a.Spec.InputSchema.Properties["choices"] = choicesSchema()
}

// choicesOf reads the choices an agent finished with, for turns where someone is there to see them.
func choicesOf(result *domain.LoopResult, unattended bool) []domain.ProposalChoice {
	if result == nil || unattended || result.Output == nil {
		return nil
	}
	return domain.ParseProposalChoices(result.Output["choices"])
}

// withNumberedChoices shows choices as a plain list, where buttons are not possible. The person can
// answer with a number or in their own words; the agent's own record of what it offered tells it which.
func withNumberedChoices(content string, choices []domain.ProposalChoice) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(content, "\n "))
	b.WriteString("\n")
	for i, c := range choices {
		fmt.Fprintf(&b, "\n%d. %s", i+1, c.Label)
	}
	return b.String()
}

// attachChoices saves the choices of the message just sent and puts the buttons under it.
func (h *Host) attachChoices(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, identity *domain.Identity, connRef string, executor domain.Executor, messageID, content string, choices []domain.ProposalChoice) {
	attacher, ok := executor.(domain.ButtonAttacher)
	if !ok || h.proposals == nil || messageID == "" || len(choices) == 0 {
		return
	}
	p := &domain.Proposal{
		ID: domain.NewID(), PersonID: conv.UserID, AgentID: hosted.Agent.ID, IdentityID: conv.IdentityID,
		ChatID: conv.ChatID, ThreadID: conv.ThreadID, MessageID: messageID, Text: content, Choices: choices,
		ExpiresAt: time.Now().Add(domain.ProposalLifetime),
	}
	if err := h.proposals.SaveProposal(ctx, p); err != nil {
		log.Printf("runtime: agent %q: could not keep the choices: %v", hosted.Agent.ID, err)
		return
	}
	buttons := make([]domain.Button, len(choices))
	for i, c := range choices {
		buttons[i] = domain.Button{Label: c.Label, Data: domain.ProposalData(p.ID, i)}
	}
	if err := attacher.AttachButtons(ctx, identity, connRef, conv, messageID, buttons); err != nil {
		log.Printf("runtime: agent %q: could not put the buttons under the message: %v", hosted.Agent.ID, err)
	}
}

// handleProposalTap handles a tap on a button made by attachChoices.
func (h *Host) handleProposalTap(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	cq := msg.CallbackQuery
	executor, ok := h.executorForMessage(hosted, msg)
	if ok {
		if ack, can := executor.(domain.CallbackQueryAcknowledger); can {
			go func() { _ = ack.AcknowledgeCallbackQuery(context.Background(), cq.QueryID) }()
		}
	}
	id, i, parsed := domain.ParseProposalData(cq.Data)
	if !parsed || h.proposals == nil {
		return
	}
	p, err := h.proposals.GetProposal(ctx, id)
	if err != nil {
		log.Printf("runtime: agent %q: tap on choices %q: %v", hosted.Agent.ID, id, err)
		return
	}
	// Only the person the choices were for, and only on the agent that made them.
	if p.AgentID != hosted.Agent.ID || msg.Conversation.UserID == "" || msg.Conversation.UserID != p.PersonID {
		log.Printf("runtime: agent %q: tap on choices %q by someone else, ignored", hosted.Agent.ID, id)
		return
	}

	conv := domain.ConversationRef{
		Provider: msg.Conversation.Provider, ChatID: p.ChatID, ThreadID: p.ThreadID, IdentityID: p.IdentityID,
		SenderID: msg.Conversation.SenderID, UserID: p.PersonID,
	}
	show := func(suffix string) {
		identity, _, connRef := h.connectionFor(ctx, hosted, conv)
		if im, can := executor.(domain.InteractiveMessenger); can && ok {
			if err := im.UpdateMessage(ctx, identity, connRef, conv, p.MessageID, p.Text+"\n\n"+suffix); err != nil {
				log.Printf("runtime: agent %q: could not update the message after a tap: %v", hosted.Agent.ID, err)
			}
		}
	}

	chosen, err := h.proposals.ChooseProposal(ctx, id, i, time.Now())
	switch {
	case err == nil:
	case errorsIs(err, domain.ErrProposalChosen):
		return // already answered; a second tap does nothing
	case errorsIs(err, domain.ErrProposalExpired):
		show("⏱ These options have expired.")
		return
	default:
		log.Printf("runtime: agent %q: tap on choices %q: %v", hosted.Agent.ID, id, err)
		return
	}
	choice := chosen.Choices[i]
	show("✅ " + choice.Label)
	// As if they had typed it: it goes through everything typing would.
	h.proposalDispatch(ctx, hosted, domain.InboundMessage{Conversation: conv, Text: choice.Say})
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// connectionFor resolves the bot and per-person connection for a conversation.
func (h *Host) connectionFor(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef) (*domain.Identity, domain.Executor, string) {
	executor, identity, connRef := h.resolveExecutorForConv(ctx, hosted, conv)
	return identity, executor, connRef
}
