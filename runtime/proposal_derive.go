package runtime

// Choices that do not depend on the model volunteering them. Some models, asked to finish with
// choices, simply never do and end "want me to…?" or "just tell me which". When a reply ends in an
// offer or a question and carries no choices, one short extra step reads the reply and turns the offer
// into specific buttons, or finds there are none to make. The reply has already been sent; the buttons
// follow it. Nothing is invented: the choices may only use what the reply itself says, and a tap is
// handled as if typed, like any other.

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// deriveTimeout bounds the extra step: buttons are a convenience and never hold anything up for long.
const deriveTimeout = 15 * time.Second

// offerEnding finds a reply that ends by asking or offering something.
var offerEnding = regexp.MustCompile(`(?i)(\?|want me to|would you like|shall i|should i|let me know|if you'd like|if you want|i can (also )?(move|book|hold|set|add|change|cancel|analy[sz]e|check|look|run|place|send|schedule|remind)|just (name|tell|give|say)|(may|might|likely|probably|will|'ll) (want|need) to|(a few|some|several) (ways|options|things)|ways to)`)

// endsInOffer reports whether the end of a reply asks or offers something.
func endsInOffer(reply string) bool {
	r := []rune(strings.TrimSpace(reply))
	if len(r) > 240 {
		r = r[len(r)-240:]
	}
	return offerEnding.MatchString(string(r))
}

const deriveInstruction = "You turn the end of an assistant's reply into buttons the person can tap. " +
	"You are given what the person wrote and the assistant's reply. " +
	"If the reply ends by offering or asking about a next step that has two to five clear answers, call offer_choices with those answers. " +
	"Each choice has a label (a few words, what the button says) and say (what the person would say if they typed it: first person, specific, " +
	"using only names, times and amounts that appear in the reply or in what the person wrote). " +
	"The label says the same thing as say, only shorter: it never drops when it would happen ('later', 'tomorrow at 9') or which one it is about, " +
	"so tapping it can't be mistaken for something else ('Check USD/CAD order' for 'Check back on my USD/CAD order later' reads as checking it now). " +
	"Offer only what the reply itself offers; never invent options, facts or actions. " +
	"Every choice is a different, concrete thing to do. Never make yes/no choices, and never 'no thanks', 'just checking', 'something else' or 'not now': " +
	"if the only thing on offer is to do one thing or not, call nothing. " +
	"If the reply simply answers, or asks something that needs the person's own words (a name, a number, a free-form detail), do not call offer_choices."

// SetChoiceModel sets the model that turns a reply's closing offer into choices. It belongs to the
// platform, not to any agent: whichever model a creator gives their agent, the buttons come out the same.
// Without it the agent's own model is used.
func (h *Host) SetChoiceModel(m domain.LLM) { h.choiceModel = m }

// DeriveChoices is the buttons the platform's model makes from a reply's closing offer, as after a real reply: for
// measuring how they come out on a real model.
func (h *Host) DeriveChoices(ctx context.Context, hosted *HostedAgent, userText, reply string) []domain.ProposalChoice {
	return h.deriveChoices(ctx, hosted, userText, reply)
}

// deriveChoices asks a model to turn a reply's closing offer into choices.
func (h *Host) deriveChoices(ctx context.Context, hosted *HostedAgent, userText, reply string) []domain.ProposalChoice {
	ctx, cancel := context.WithTimeout(ctx, deriveTimeout)
	defer cancel()
	spec := domain.ActionSpec{
		Name:        "offer_choices",
		Description: "Shows these as buttons under the assistant's reply.",
		Hidden:      true,
		InputSchema: domain.Schema{
			Type:       domain.SchemaTypeObject,
			Properties: map[string]domain.Schema{"choices": choicesSchema()},
			Required:   []string{"choices"},
		},
	}
	msgs := []domain.Message{
		{Role: domain.RoleSystem, Content: deriveInstruction},
		{Role: domain.RoleUser, Content: "The person wrote:\n" + userText + "\n\nThe assistant replied:\n" + reply},
	}
	models := hosted.Agent.LLMs
	if h.choiceModel != nil {
		models = []domain.LLM{h.choiceModel}
	}
	var lastErr error
	for _, llm := range models {
		resp, err := llm.Call(ctx, msgs, []domain.ActionSpec{spec})
		if err != nil {
			lastErr = err
			continue
		}
		for _, call := range resp.ToolCalls {
			if call.Name == spec.Name {
				return domain.ParseProposalChoices(call.Arguments["choices"])
			}
		}
		return nil
	}
	if lastErr != nil {
		log.Printf("runtime: agent %q: could not make choices from the reply: %v", hosted.Agent.ID, lastErr)
	}
	return nil
}

// withDerived returns the choices to show under a reply: the ones the agent gave, or, when it gave none
// and the reply ends in an offer, the ones made from the reply.
func (h *Host) withDerived(ctx context.Context, hosted *HostedAgent, result *domain.LoopResult, userText, reply string, given []domain.ProposalChoice) []domain.ProposalChoice {
	if len(given) > 0 || result == nil || result.Status != domain.LoopStatusComplete || result.Content == "" || reply != result.Content {
		return given
	}
	if (h.choiceModel == nil && len(hosted.Agent.LLMs) == 0) || !endsInOffer(reply) || !h.canOfferChoices(ctx, hosted) {
		return given
	}
	return h.deriveChoices(ctx, hosted, userText, reply)
}
