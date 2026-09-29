package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"strings"

	"github.com/unitz007/open-kael/domain"
)

// ListenAndServe starts one Listener goroutine per (agent, identity) pair
// whose Executor implements Listener, and one processing goroutine per agent
// that serializes every inbound message for that agent through HandleTurn
// one at a time. Blocks until ctx is cancelled and all goroutines return.
func (h *Host) ListenAndServe(ctx context.Context) error {
	h.serveCtx = ctx

	// Sentinel: keeps serveWG non-zero until ctx is cancelled so dynamically
	// added listeners can safely call h.serveWG.Add without racing with Wait.
	h.serveWG.Add(1)
	go func() {
		defer h.serveWG.Done()
		<-ctx.Done()
	}()

	h.mu.RLock()
	hostedAgents := make([]*HostedAgent, 0, len(h.agents))
	for _, hosted := range h.agents {
		hostedAgents = append(hostedAgents, hosted)
	}
	h.mu.RUnlock()

	for _, hosted := range hostedAgents {
		inbox := make(chan domain.InboundMessage, 64)
		h.agentInboxes.Store(hosted.Agent.ID, inbox)

		if h.startListeners(ctx, hosted, inbox) == 0 {
			continue
		}

		h.serveWG.Add(1)
		go func(hosted *HostedAgent, inbox chan domain.InboundMessage) {
			defer h.serveWG.Done()
			for {
				select {
				case msg := <-inbox:
					h.handleInboundSafely(ctx, hosted, msg)
				case <-ctx.Done():
					return
				}
			}
		}(hosted, inbox)
	}

	h.serveWG.Wait()
	return nil
}

// startListeners launches one goroutine per Listener-capable Executor for
// each Identity the agent uses, each feeding inbox. Returns how many were
// started — 0 means no listeners so the caller should skip the processing
// goroutine.
func (h *Host) startListeners(ctx context.Context, hosted *HostedAgent, inbox chan domain.InboundMessage) int {
	started := 0
	for _, identityID := range hosted.Agent.IdentityIDs {
		identity, ok := hosted.Deps.IdentitiesByID[identityID]
		if !ok {
			continue
		}
		integration, ok := hosted.Deps.IntegrationsByID[identity.IntegrationID]
		if !ok {
			continue
		}
		executor, ok := hosted.Deps.Executors.For(integration.Service)
		if !ok {
			continue
		}
		listener, ok := executor.(domain.Listener)
		if !ok {
			continue
		}
		started++
		h.startOneListener(ctx, hosted.Agent.ID, identity, listener, inbox)
	}
	return started
}

// startOneListener starts a single listener goroutine for identity, feeding
// messages into inbox. Registers a cancel func in listenerCancels so
// StopIdentityListeners can stop it later.
func (h *Host) startOneListener(ctx context.Context, agentID string, identity *domain.Identity, listener domain.Listener, inbox chan domain.InboundMessage) {
	listenerCtx, cancel := context.WithCancel(ctx)
	key := agentID + ":" + identity.ID
	h.listenerCancels.Store(key, cancel)
	h.serveWG.Add(1)
	go func(id *domain.Identity, l domain.Listener, lCtx context.Context, lCancel context.CancelFunc, lKey string) {
		defer h.serveWG.Done()
		defer h.listenerCancels.Delete(lKey)
		defer lCancel()
		defer recoverFromPanic(agentID, "listener "+id.ID)
		if err := l.Listen(lCtx, id, func(msg domain.InboundMessage) {
			select {
			case inbox <- msg:
			case <-lCtx.Done():
			}
		}); err != nil && lCtx.Err() == nil {
			log.Printf("runtime: agent %q: listener %q stopped: %v", agentID, id.ID, err)
		}
	}(identity, listener, listenerCtx, cancel, key)
}

// StartListenerForAgent starts a listener goroutine for the given identity on
// agentID. If the agent has no inbox yet (it had no listeners at boot), one is
// created along with a processing goroutine. The HostedAgent's in-memory Deps
// are updated so replies can be routed back through the new identity.
// No-op when ListenAndServe is not running or the identity's executor doesn't
// implement domain.Listener.
func (h *Host) StartListenerForAgent(agentID string, identity *domain.Identity, integration *domain.Integration) {
	if h.serveCtx == nil {
		return
	}
	ctx := h.serveCtx

	h.mu.Lock()
	hosted, ok := h.agents[agentID]
	if ok {
		if hosted.Deps.IdentitiesByID == nil {
			hosted.Deps.IdentitiesByID = make(map[string]*domain.Identity)
		}
		if hosted.Deps.IntegrationsByID == nil {
			hosted.Deps.IntegrationsByID = make(map[string]*domain.Integration)
		}
		hosted.Deps.IdentitiesByID[identity.ID] = identity
		hosted.Deps.IntegrationsByID[integration.ID] = integration
		found := false
		for _, id := range hosted.Agent.IdentityIDs {
			if id == identity.ID {
				found = true
				break
			}
		}
		if !found {
			hosted.Agent.IdentityIDs = append(hosted.Agent.IdentityIDs, identity.ID)
		}
	}
	h.mu.Unlock()

	if !ok {
		return
	}

	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return
	}
	listener, ok := executor.(domain.Listener)
	if !ok {
		return
	}

	// Get the existing inbox, or create one and start a processing goroutine.
	newInbox := make(chan domain.InboundMessage, 64)
	actual, loaded := h.agentInboxes.LoadOrStore(agentID, newInbox)
	inbox := actual.(chan domain.InboundMessage)
	if !loaded {
		h.serveWG.Add(1)
		go func() {
			defer h.serveWG.Done()
			for {
				select {
				case msg := <-inbox:
					h.handleInboundSafely(ctx, hosted, msg)
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	h.startOneListener(ctx, agentID, identity, listener, inbox)
}

// DispatchInbound is the public entry point for injecting an inbound message
// into the host's processing pipeline — the same path ListenAndServe uses
// internally. Useful for testing and for any external trigger that produces
// a message without going through a registered Listener.
func (h *Host) DispatchInbound(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	h.handleInboundSafely(ctx, hosted, msg)
}

// pendingCredential tracks the state of an in-progress in-bot OAuth connection
// flow where the user must click a link and paste back the redirect URL.
type pendingCredential struct {
	userID     string
	identityID string
	step       string // "awaiting_url"
}

func (h *Host) handleInboundSafely(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	defer recoverFromPanic(hosted.Agent.ID, "handling inbound message")
	if msg.ThreadID != "" {
		msg.Conversation.ThreadID = msg.ThreadID
	}

	// Route callback queries (button taps) directly — they never go to HandleTurn.
	if msg.CallbackQuery != nil {
		h.handleCallbackQuery(ctx, hosted, msg)
		return
	}

	// Intercept channel link codes before routing to HandleTurn. Handles both
	// plain codes and Telegram's /start <code> deep link format.
	if h.channelRedeemer != nil {
		if code := extractLinkCode(msg.Text); code != "" {
			userID, err := h.channelRedeemer(ctx, code, msg.Conversation.IdentityID, msg.Conversation.ChatID)
			if err == nil {
				payload, _ := json.Marshal(map[string]string{
					"user_id":     userID,
					"identity_id": msg.Conversation.IdentityID,
					"channel_ref": msg.Conversation.ChatID,
				})
				h.Emit(ctx, "user.channel.connected", string(payload))
				// Send setup link so the user can configure integrations.
				if h.frontendURL != "" {
					setupURL := h.frontendURL + "/setup"
					extra := map[string]any{
						"link_button": map[string]any{
							"text": "Set up integrations",
							"url":  setupURL,
						},
					}
					h.deliverBestEffort(ctx, hosted, msg.Conversation,
						"Your account is now linked! Tap below to configure your integrations.", extra)
				}
				return
			}
			if !errors.Is(err, domain.ErrNotFound) {
				log.Printf("runtime: agent %q: channel redemption identity %s chat %s: %v",
					hosted.Agent.ID, msg.Conversation.IdentityID, msg.Conversation.ChatID, err)
			}
			// ErrNotFound means it's not a link code — fall through normally.
		}
	}

	// Resolve the platform user from the messenger identity.
	if h.userChannelResolver != nil && msg.Conversation.UserID == "" {
		if userID, err := h.userChannelResolver(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID); err == nil {
			msg.Conversation.UserID = userID
		}
	}

	// Unlinked user — auto-provision when possible, otherwise send a link code.
	if msg.Conversation.UserID == "" {
		if h.autoProvisioner != nil {
			userID, err := h.autoProvisioner(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID)
			if err == nil {
				msg.Conversation.UserID = userID
			} else {
				log.Printf("runtime: agent %q: auto-provision identity %s chat %s: %v",
					hosted.Agent.ID, msg.Conversation.IdentityID, msg.Conversation.ChatID, err)
			}
		}

		if msg.Conversation.UserID == "" {
			// Fallback: generate a link code and ask the user to connect via web.
			var linkCode string
			if h.linkCodeGenerator != nil {
				if code, err := h.linkCodeGenerator(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID); err == nil {
					linkCode = code
				} else {
					log.Printf("runtime: agent %q: generate link code identity %s chat %s: %v",
						hosted.Agent.ID, msg.Conversation.IdentityID, msg.Conversation.ChatID, err)
				}
			}
			text, linkURL := h.onboardingMessage(ctx, hosted.Agent, hosted.Agent.LLMs, linkCode)
			var extra map[string]any
			if linkURL != "" {
				extra = map[string]any{
					"link_button": map[string]any{
						"text": "Connect account",
						"url":  linkURL,
					},
				}
			}
			h.deliverBestEffort(ctx, hosted, msg.Conversation, text, extra)
			return
		}
	}

	// Key for the pending credential collection state machine.
	setupKey := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID

	// If a credential collection conversation is in progress for this chat,
	// route the message there instead of HandleTurn.
	if ps, ok := h.pendingSetups.Load(setupKey); ok {
		h.handlePendingSetup(ctx, hosted, msg, ps.(*pendingCredential), setupKey)
		return
	}

	// Check whether the user still needs to connect any integrations.
	if h.setupChecker != nil {
		missingIDs, err := h.setupChecker(ctx, msg.Conversation.UserID, hosted.Agent)
		if err != nil {
			log.Printf("runtime: agent %q: setup check user %s: %v", hosted.Agent.ID, msg.Conversation.UserID, err)
		} else if len(missingIDs) > 0 {
			for _, identityID := range missingIDs {
				var connectURL string
				if h.connectURLGenerator != nil {
					if u, err := h.connectURLGenerator(ctx, msg.Conversation.UserID, identityID); err == nil {
						connectURL = u
					}
				}
				if connectURL == "" {
					continue
				}
				var extra map[string]any
				if !isLocalhostURL(connectURL) {
					extra = map[string]any{
						"web_app_button": map[string]any{
							"text": "Connect FPL account",
							"url":  connectURL,
						},
					}
				}
				h.deliverBestEffort(ctx, hosted, msg.Conversation,
					"To use FPL features, tap below to connect your Fantasy Premier League account.", extra)
				return
			}
			return
		}
	}

	// Settings menu command: intercept before the LLM turn loop.
	if h.settingsFlow != nil && isSettingsCommand(msg.Text) {
		if executor, ok := h.executorForMessage(hosted, msg); ok {
			if provider, ok := executor.(domain.SettingsMenuProvider); ok {
				h.settingsFlow.Open(ctx, provider, hosted, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.UserID)
				return
			}
		}
	}

	if _, err := h.HandleTurn(ctx, hosted, msg.Conversation, msg.Text); err != nil {
		log.Printf("runtime: agent %q: handling message from identity %s chat %s: %v", hosted.Agent.ID, msg.Conversation.IdentityID, msg.Conversation.ChatID, err)
	}
}

// handlePendingSetup handles a message in an in-progress OAuth URL-paste flow.
// The user is expected to paste the redirect URL they landed on after signing in.
func (h *Host) handlePendingSetup(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage, ps *pendingCredential, key string) {
	if ps.step != "awaiting_url" {
		return
	}
	text := strings.TrimSpace(msg.Text)
	parsed, err := url.Parse(text)
	if err != nil {
		h.deliverBestEffort(ctx, hosted, msg.Conversation,
			"That doesn't look like a valid URL. Please copy the full URL from your browser's address bar after signing in and paste it here.")
		return
	}
	code := parsed.Query().Get("code")
	state := parsed.Query().Get("state")
	if code == "" || state == "" {
		h.deliverBestEffort(ctx, hosted, msg.Conversation,
			"Couldn't find the authorization code in that URL. Make sure you copied the full URL from your browser after signing in.")
		return
	}
	h.pendingSetups.Delete(key)
	if h.credentialSaver == nil {
		h.deliverBestEffort(ctx, hosted, msg.Conversation, "FPL connection is not configured on this server.")
		return
	}
	if err := h.credentialSaver(ctx, ps.userID, ps.identityID, code, state); err != nil {
		log.Printf("runtime: agent %q: save FPL credential user %s identity %s: %v", hosted.Agent.ID, ps.userID, ps.identityID, err)
		h.deliverBestEffort(ctx, hosted, msg.Conversation,
			"Couldn't connect your FPL account — the link may have expired. Send any message to try again.")
		return
	}
	h.deliverBestEffort(ctx, hosted, msg.Conversation,
		"Your FPL account is now connected! You can start asking about your team.")
}

// handleCallbackQuery dispatches a button-tap InboundMessage. It resolves the
// user, then routes kael_sm: data to the SettingsFlow if the executor
// implements SettingsMenuProvider. Approval callbacks (kael_approve:/kael_reject:)
// are left to the executor's own internal handling (via the listener loop).
func (h *Host) handleCallbackQuery(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	cq := msg.CallbackQuery

	// Resolve the platform user — same as the normal message path.
	if h.userChannelResolver != nil && msg.Conversation.UserID == "" {
		if userID, err := h.userChannelResolver(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID); err == nil {
			msg.Conversation.UserID = userID
		}
	}

	if strings.HasPrefix(cq.Data, "kael_sm:") && h.settingsFlow != nil {
		executor, ok := h.executorForMessage(hosted, msg)
		if !ok {
			return
		}
		provider, ok := executor.(domain.SettingsMenuProvider)
		if !ok {
			return
		}
		action := cq.Data[len("kael_sm:"):]
		h.settingsFlow.Handle(ctx, provider, hosted,
			msg.Conversation.IdentityID, msg.Conversation.ChatID,
			cq.MessageID, msg.Conversation.UserID, action)

		// Dismiss the loading spinner on the client after the action completes.
		if ack, ok := executor.(domain.CallbackQueryAcknowledger); ok {
			go func() { _ = ack.AcknowledgeCallbackQuery(context.Background(), cq.QueryID) }()
		}
	}
}

// isSettingsCommand reports whether text is a settings-menu trigger.
func isSettingsCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return t == "/settings" || t == "settings"
}

// executorForMessage returns the Executor registered for the integration that
// owns msg.Conversation.IdentityID, if one exists.
func (h *Host) executorForMessage(hosted *HostedAgent, msg domain.InboundMessage) (domain.Executor, bool) {
	identity, ok := hosted.Deps.IdentitiesByID[msg.Conversation.IdentityID]
	if !ok {
		return nil, false
	}
	integration, ok := hosted.Deps.IntegrationsByID[identity.IntegrationID]
	if !ok {
		return nil, false
	}
	return hosted.Deps.Executors.For(integration.Service)
}

// extractLinkCode extracts a potential ChannelLinkCode from a message text.
// Handles Telegram's /start <code> deep-link format and plain token messages.
// Returns empty string when the text is clearly not a code (contains spaces,
// too short, or too long) to avoid a redundant DB lookup on every message.
func extractLinkCode(text string) string {
	text = strings.TrimSpace(text)
	if after, ok := strings.CutPrefix(text, "/start "); ok {
		return strings.TrimSpace(after)
	}
	// Plain token: no spaces, length in the range a random token would occupy.
	if !strings.Contains(text, " ") && len(text) >= 16 && len(text) <= 128 {
		return text
	}
	return ""
}

func recoverFromPanic(agentID, where string) {
	if r := recover(); r != nil {
		log.Printf("runtime: agent %q: recovered panic in %s: %v", agentID, where, r)
	}
}
