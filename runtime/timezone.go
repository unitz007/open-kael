package runtime

import (
	"context"
	"errors"
	"log"

	"github.com/unitz007/open-kael/domain"
)

// SetTimezoneLink turns on asking for a time zone. link makes the address of a page that sets it with
// one tap (the person's own device knows it), for the messenger the person is on (provider, e.g.
// "telegram"). Where it returns an address, a turn whose person has no time zone yet can ask for it
// the moment something depends on their clock; where it returns "" (a messenger that can't show the
// button) nothing asks, and times fall back to UTC as before.
func (h *Host) SetTimezoneLink(link func(ctx context.Context, userID, agentID, provider string) (string, error)) {
	h.timezoneLink = link
}

var errNoTimezonePrompt = errors.New("no way to ask for a time zone here")

const timezonePrompt = "To get the times right I need to know your time zone. Tap the button below: it takes one tap, and then I'll carry on with what you asked."

// timezoneRequester asks one person for their time zone: it sends the prompt with the button, keeps
// what they had just asked so it is carried on once they answer, and never sends the prompt twice
// in a short while.
func (h *Host) timezoneRequester(hosted *HostedAgent) domain.TimezoneRequester {
	return timezoneRequesterFunc(func(ctx context.Context, conv domain.ConversationRef, userID string) error {
		if userID == "" {
			return errNoTimezonePrompt
		}
		link, err := h.timezoneLink(ctx, userID, hosted.Agent.ID, conv.Provider)
		if err != nil {
			return err
		}
		if link == "" {
			return errNoTimezonePrompt // this messenger can't show the one-tap prompt: nothing waits for it
		}
		// What they asked is kept every time, even when the prompt itself is not repeated.
		if msg := domain.OriginalMessageFromContext(ctx); msg != "" {
			h.holdTurn(ctx, userID, domain.TimezoneGateIdentity, pendingAuthTurn{conv: conv, agentID: hosted.Agent.ID, userMessage: msg})
		} else {
			log.Printf("runtime: time zone: no original message in context — it will NOT be carried on (user %q)", userID)
		}
		if !h.shouldSendConnectPrompt("timezone:" + userID) {
			return nil
		}
		text := timezonePrompt
		var extra map[string]any
		if isLocalhostURL(link) {
			text += "\n\n" + link
		} else {
			extra = map[string]any{"web_app_button": map[string]any{"text": "Set my time zone", "url": link}}
		}
		h.deliverBestEffort(ctx, hosted, conv, text, extra)
		return nil
	})
}

type timezoneRequesterFunc func(ctx context.Context, conv domain.ConversationRef, userID string) error

func (f timezoneRequesterFunc) RequestTimezone(ctx context.Context, conv domain.ConversationRef, userID string) error {
	return f(ctx, conv, userID)
}

// OnTimezoneSet carries on with what a person asked before they were asked for their time zone, now
// that it is known. It does nothing when nothing was waiting.
func (h *Host) OnTimezoneSet(ctx context.Context, userID string) {
	pending, ok := h.takeTurn(ctx, userID, domain.TimezoneGateIdentity)
	if !ok {
		return
	}
	h.mu.RLock()
	hosted := h.agents[pending.agentID]
	h.mu.RUnlock()
	if hosted == nil {
		return
	}
	log.Printf("runtime: time zone set for user %q: carrying on with their request", userID)
	replay := domain.WithOriginalMessage(context.Background(), pending.userMessage)
	go func() {
		if _, err := h.HandleTurn(replay, hosted, pending.conv, pending.userMessage); err != nil {
			log.Printf("runtime: carry on after time zone for user %q: %v", userID, err)
		}
	}()
}
