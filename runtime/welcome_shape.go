package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// WelcomeShape gives the parts of an agent's welcome the platform knows best: what it helps with, in a few plain words
// ("your fantasy football team, email and calendar"), and things to try asking first (one per kind of app it has).
type WelcomeShape func(ctx context.Context, agent *domain.Agent) (helpsWith string, tryAsking []string)

// SetWelcomeShape sets what an agent's welcome is made from (see WelcomeShape). Without one, the welcome lists what it
// can do, as before.
func (h *Host) SetWelcomeShape(f WelcomeShape) { h.welcomeShape = f }

func (h *Host) shapeOf(ctx context.Context, agent *domain.Agent) (string, []string) {
	if h.welcomeShape == nil || agent == nil {
		return "", nil
	}
	return h.welcomeShape(ctx, agent)
}

// greetingFor is the first thing a new person is told: the creator's own greeting word for word, else who it is and
// what it helps with in a line ("I can help with your fantasy football team, email and calendar. Just tell me what you
// need."), else (with no shape) what it can do, a line each.
func (h *Host) greetingFor(ctx context.Context, agent *domain.Agent) string {
	if agent.Greeting != "" {
		return agent.Greeting
	}
	if helps, _ := h.shapeOf(ctx, agent); strings.TrimSpace(helps) != "" {
		return "👋 Hi! I'm " + agent.Name + ". I can help with " + strings.TrimSpace(helps) + ". Just tell me what you need."
	}
	return greetingBody(agent, h.abilitiesOf(ctx, agent))
}

// linkElsewhereKey is the key under a new person's welcome for someone who already uses the agent on its web link.
func linkElsewhereKey(agent *domain.Agent) string {
	return "I already use " + agent.Name + " on the web"
}

// onWebLink reports whether an agent has a web link (a chat app at an address).
func onWebLink(hosted *HostedAgent) bool {
	for _, id := range hosted.Agent.IdentityIDs {
		if identity, ok := hosted.catalog().Identity(id); ok {
			if integration, ok := hosted.catalog().Integration(identity.IntegrationID); ok && integration.Service == "web" {
				return true
			}
		}
	}
	return false
}

// welcomeKeys are the taps a new person's welcome carries above the keyboard on a chat app that can show them: things to
// try asking, and, when the agent has a web link and emails can be sent, "I already use … on the web".
func (h *Host) welcomeKeys(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef) []string {
	if conv.Provider == "web" {
		return nil
	}
	_, asks := h.shapeOf(ctx, hosted.Agent)
	keys := append([]string(nil), asks...)
	if len(keys) > 3 {
		keys = keys[:3]
	}
	if h.emailLinkInitiator != nil && onWebLink(hosted) {
		keys = append(keys, linkElsewhereKey(hosted.Agent))
	}
	return keys
}

// takeLinkElsewhere: "I already use … on the web" (the welcome's key) starts joining this chat to the person's other
// one: it asks for the email they use there and sends the link that asks (the email link flow).
func (h *Host) takeLinkElsewhere(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) bool {
	if h.emailLinkInitiator == nil || msg.Conversation.UserID == "" || !strings.EqualFold(strings.TrimSpace(msg.Text), linkElsewhereKey(hosted.Agent)) {
		return false
	}
	key := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID
	h.pendingEmailLinks.Store(key, time.Now())
	if h.emailLinkStateSetter != nil {
		_ = h.emailLinkStateSetter(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, "awaiting")
	}
	h.deliverBestEffort(ctx, hosted, msg.Conversation, "Great: what's the email you use with "+hosted.Agent.Name+
		" there? I'll send you a link that joins this chat to it. (Or type skip.)")
	return true
}
