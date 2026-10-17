package runtime

import (
	"context"
	"log"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/unitz007/open-kael/domain"
)

// pendingTurnMaxAge is how long a held request stays good. Past it, the person is
// asked again: an instruction from hours ago must not be carried out as if it were
// fresh (it might be a trade).
const pendingTurnMaxAge = 30 * time.Minute

// PendingTurnStore keeps what a person asked for while an account had to be connected,
// so it survives a restart and can be carried out once they connect.
type PendingTurnStore interface {
	SavePendingTurn(ctx context.Context, t *domain.PendingTurn) error
	// TakePendingTurn returns and removes the held request, or domain.ErrNotFound when
	// there is none or it is older than maxAge.
	TakePendingTurn(ctx context.Context, userID, identityID string, maxAge time.Duration) (*domain.PendingTurn, error)
}

// SetPendingTurnStore makes held requests durable. Without it they are kept in memory
// only (lost on restart).
func (h *Host) SetPendingTurnStore(s PendingTurnStore) { h.pendingTurnStore = s }

// SetUserTimezoneLoader registers how to look up a person's IANA time zone name
// ("" when unknown), so agents can speak in their local time.
func (h *Host) SetUserTimezoneLoader(f func(ctx context.Context, userID string) (string, error)) {
	h.userTimezoneLoader = f
}

// holdTurn keeps a request until the person connects what it needs.
func (h *Host) holdTurn(ctx context.Context, userID, identityID string, p pendingAuthTurn) {
	if h.pendingTurnStore != nil {
		err := h.pendingTurnStore.SavePendingTurn(ctx, &domain.PendingTurn{
			UserID: userID, IdentityID: identityID, AgentID: p.agentID, Conv: p.conv, Message: p.userMessage,
		})
		if err == nil {
			return
		}
		log.Printf("runtime: hold turn user %s identity %s: %v (keeping it in memory instead)", userID, identityID, err)
	}
	h.pendingAuthTurns.Store(userID+":"+identityID, p)
}

// takeTurn returns and removes the held request, if there is a fresh one.
func (h *Host) takeTurn(ctx context.Context, userID, identityID string) (pendingAuthTurn, bool) {
	if h.pendingTurnStore != nil {
		t, err := h.pendingTurnStore.TakePendingTurn(ctx, userID, identityID, pendingTurnMaxAge)
		if err == nil {
			return pendingAuthTurn{conv: t.Conv, agentID: t.AgentID, userMessage: t.Message}, true
		}
		if err != domain.ErrNotFound {
			log.Printf("runtime: take held turn user %s identity %s: %v", userID, identityID, err)
		}
	}
	if val, ok := h.pendingAuthTurns.LoadAndDelete(userID + ":" + identityID); ok {
		return val.(pendingAuthTurn), true
	}
	return pendingAuthTurn{}, false
}

// NeededConnection is an account the agent needs the person to connect.
type NeededConnection struct {
	IdentityID string
	Name       string // what to call it, e.g. "Aegis"; may be empty
	URL        string // where the person goes to connect it
}

// ConnectionsNeeded lists what the person still has to connect for this agent, each
// with the link to do it. Empty when they are all set.
func (h *Host) ConnectionsNeeded(ctx context.Context, userID string, agent *domain.Agent) []NeededConnection {
	if h.setupChecker == nil || h.connectURLGenerator == nil || userID == "" {
		return nil
	}
	missing, err := h.setupChecker(ctx, userID, agent)
	if err != nil {
		log.Printf("runtime: connections needed user %s agent %s: %v", userID, agent.ID, err)
		return nil
	}
	var out []NeededConnection
	for _, id := range missing {
		link, err := h.connectURLGenerator(ctx, userID, id)
		if err != nil || link == "" {
			continue
		}
		n := NeededConnection{IdentityID: id, URL: link}
		if h.connectIntegrationNameLoader != nil {
			if name, err := h.connectIntegrationNameLoader(ctx, id); err == nil {
				n.Name = name
			}
		}
		out = append(out, n)
	}
	return out
}

// gettingStartedButton is the "Get started" button added to a new Telegram person's
// welcome: it opens the short screen where they say their name and time zone and
// connect what they need. Nil when there is no Mini App to open (other messengers, or
// no public address — Telegram refuses a local one).
func (h *Host) gettingStartedButton(conv domain.ConversationRef, agentID string) map[string]any {
	if h.miniAppBaseURL == "" || conv.Provider != "telegram" || isLocalhostURL(h.miniAppBaseURL) {
		return nil
	}
	return map[string]any{
		"web_app_button": map[string]any{
			"text": "Get started",
			"url":  h.miniAppBaseURL + "/miniapp/welcome?agent=" + url.QueryEscape(agentID),
		},
	}
}

// GreetingJudge says whether a message is nothing but a hello. It lets the host skip
// the model for a first "Hi" so a new person isn't greeted twice.
type GreetingJudge interface {
	IsJustGreeting(ctx context.Context, text string) (bool, error)
}

// SetGreetingJudge registers how to tell a bare hello from a real message.
func (h *Host) SetGreetingJudge(j GreetingJudge) { h.greetingJudge = j }

// maxGreetingRunes: a longer message is never "just a hello", so it is not even asked about.
const maxGreetingRunes = 60

// onlySaysHello reports whether a new person's first message needs no answer beyond the
// welcome they have just been sent. Telegram's own /start always counts; a long message
// never does; anything else is put to the judge. When in doubt (no judge, or it can't say)
// the answer is no, so a real request is never left unanswered.
func (h *Host) onlySaysHello(ctx context.Context, text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || text == "/start" || strings.HasPrefix(text, "/start ") {
		return true
	}
	if h.greetingJudge == nil || utf8.RuneCountInString(text) > maxGreetingRunes {
		return false
	}
	just, err := h.greetingJudge.IsJustGreeting(ctx, text)
	if err != nil {
		log.Printf("runtime: greeting check: %v (answering the message instead)", err)
		return false
	}
	return just
}

// WelcomeMessageStore remembers which message carries a person's "Get started" button,
// so the button can be taken off once they are done.
type WelcomeMessageStore interface {
	SaveWelcomeMessage(ctx context.Context, identityID, channelRef, messageID string) error
	// TakeWelcomeMessage returns the remembered message ID and forgets it ("" if none).
	TakeWelcomeMessage(ctx context.Context, identityID, channelRef string) (string, error)
}

// SetWelcomeMessageStore turns on removing the "Get started" button once they are done.
func (h *Host) SetWelcomeMessageStore(s WelcomeMessageStore) { h.welcomeMessageStore = s }

// RemoveGettingStartedButton takes the "Get started" button off the person's welcome
// message. Safe to call more than once and when there is no button: it only ever acts
// on a message it remembers, and forgets it first. Best effort: a messenger that can't
// remove buttons, or a message that is gone, is not an error worth stopping for.
func (h *Host) RemoveGettingStartedButton(ctx context.Context, agentID, identityID, channelRef string) {
	if h.welcomeMessageStore == nil {
		return
	}
	messageID, err := h.welcomeMessageStore.TakeWelcomeMessage(ctx, identityID, channelRef)
	if err != nil || messageID == "" {
		if err != nil {
			log.Printf("runtime: take welcome message %s/%s: %v", identityID, channelRef, err)
		}
		return
	}
	hosted, ok := h.Get(agentID)
	if !ok {
		return
	}
	conv := domain.ConversationRef{IdentityID: identityID, ChatID: channelRef}
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		return
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return
	}
	var connectionRef string
	if identity != nil {
		connectionRef, _ = domain.ConnectionRefFromContext(ctx, identity.ID)
	}
	if _, err := executor.Execute(ctx, identity, connectionRef, domain.ActionRemoveButtons,
		map[string]any{"recipient": channelRef, "message_id": messageID}); err != nil {
		log.Printf("runtime: remove welcome button %s/%s: %v", identityID, channelRef, err)
	}
}
