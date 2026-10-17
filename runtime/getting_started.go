package runtime

import (
	"context"
	"log"
	"net/url"
	"time"

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
