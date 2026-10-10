package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

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
const (
	listenerBackoffMin = 2 * time.Second
	listenerBackoffMax = 2 * time.Minute
)

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
		backoff := listenerBackoffMin
		for {
			err := l.Listen(lCtx, id, func(msg domain.InboundMessage) {
				select {
				case inbox <- msg:
				case <-lCtx.Done():
				}
			})
			if lCtx.Err() != nil {
				return // clean shutdown
			}
			if err != nil {
				if errors.Is(err, domain.ErrPermanent) {
					log.Printf("runtime: agent %q: listener %q stopped permanently: %v", agentID, id.ID, err)
					return
				}
				log.Printf("runtime: agent %q: listener %q stopped: %v — restarting in %s", agentID, id.ID, err, backoff)
				select {
				case <-time.After(backoff):
				case <-lCtx.Done():
					return
				}
				if backoff < listenerBackoffMax {
					backoff *= 2
				}
				continue
			}
			return // listener returned nil (clean exit without ctx cancel — shouldn't normally happen)
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

// pendingInstructionsState tracks a chat that is waiting for the user's
// personal instructions text reply (after a ForceReply or text-based prompt).
type pendingInstructionsState struct {
	userID  string
	agentID string
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

	// Handle modal submissions (e.g. Slack views.open for personal instructions).
	if msg.InstructionsSubmission != nil {
		h.handleInstructionsSubmission(ctx, hosted, msg)
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
		if userID, err := h.userChannelResolver(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.SenderID); err == nil {
			msg.Conversation.UserID = userID
		}
	}

	// Unlinked user — auto-provision when possible, otherwise send a link code.
	if msg.Conversation.UserID == "" {
		if h.autoProvisioner != nil {
			userID, err := h.autoProvisioner(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.SenderID)
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

	// Onboarding intercept: runs before the integration setup checker so new
	// users introduce themselves before being asked to connect any accounts.
	if h.onboardingChecker != nil && h.onboardingCompleter != nil && msg.Conversation.UserID != "" {
		setupKey := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID

		// Check pending: in-memory fast path first, then DB fallback so the
		// state survives a server restart between prompt and reply.
		_, inMemPending := h.pendingOnboardings.Load(setupKey)
		dbPending := false
		if !inMemPending && h.onboardingPromptedChecker != nil {
			if p, err := h.onboardingPromptedChecker(ctx, msg.Conversation.UserID, msg.Conversation.IdentityID, msg.Conversation.ChatID); err == nil {
				dbPending = p
			} else {
				log.Printf("runtime: agent %q: onboarding prompted checker: %v", hosted.Agent.ID, err)
			}
		}

		if inMemPending || dbPending {
			// User is replying to the onboarding prompt — save their intro.
			h.pendingOnboardings.Delete(setupKey)
			if err := h.onboardingCompleter(ctx,
				msg.Conversation.IdentityID, msg.Conversation.ChatID,
				hosted.Agent.ID, msg.Conversation.UserID, msg.Text,
			); err != nil {
				log.Printf("runtime: agent %q: onboarding completer: %v", hosted.Agent.ID, err)
			}
			ack := h.onboardingAck(ctx, hosted.Agent, hosted.Agent.LLMs, msg.Text)
			h.deliverBestEffort(ctx, hosted, msg.Conversation, ack)
			return
		}

		onboarded, err := h.onboardingChecker(ctx, msg.Conversation.UserID, msg.Conversation.IdentityID, msg.Conversation.ChatID)
		if err != nil {
			log.Printf("runtime: agent %q: onboarding checker: %v", hosted.Agent.ID, err)
		}
		if !onboarded {
			var pubSkills []*domain.Skill
			for _, s := range hosted.Agent.Skills {
				if s.Visibility == domain.SkillPublic {
					pubSkills = append(pubSkills, s)
				}
			}
			greeting := h.greetingBody(ctx, hosted.Agent, pubSkills, hosted.Agent.LLMs)
			if h.settingsFlow != nil {
				greeting += "\n\nType /settings to manage your preferences or link your account across platforms."
			}
			// Complete onboarding immediately — no "tell me about yourself" step.
			if h.onboardingCompleter != nil {
				if err := h.onboardingCompleter(ctx,
					msg.Conversation.IdentityID, msg.Conversation.ChatID,
					hosted.Agent.ID, msg.Conversation.UserID, "",
				); err != nil {
					log.Printf("runtime: agent %q: onboarding completer: %v", hosted.Agent.ID, err)
				}
			}
			h.deliverBestEffort(ctx, hosted, msg.Conversation, greeting)
			return
		}
	}

	// Email-link flow: runs after onboarding so the user is introduced first.
	// Prompts for an email address to enable cross-platform identity linking.
	// Entirely optional — "skip" opts out permanently on this channel.
	if h.emailLinkChecker != nil && msg.Conversation.UserID != "" {
		emailKey := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID

		_, inMemAwaiting := h.pendingEmailLinks.Load(emailKey)

		// If not in memory, check DB state (handles restarts between prompt and reply).
		if !inMemAwaiting && h.emailLinkStateGetter != nil {
			if s, err := h.emailLinkStateGetter(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID); err == nil {
				inMemAwaiting = s == "awaiting"
			}
		}

		if inMemAwaiting {
			h.pendingEmailLinks.Delete(emailKey)
			text := strings.TrimSpace(msg.Text)

			if strings.EqualFold(text, "skip") {
				if h.emailLinkStateSetter != nil {
					_ = h.emailLinkStateSetter(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, "skipped")
				}
				h.deliverBestEffort(ctx, hosted, msg.Conversation, "No problem — you can always link your account later by sharing your email.")
				return
			}

			email := extractEmail(text)
			if email == "" {
				// Re-arm the in-memory awaiting so the next message is treated as the email too.
				h.pendingEmailLinks.Store(emailKey, true)
				h.deliverBestEffort(ctx, hosted, msg.Conversation, "That doesn't look like a valid email address. Please try again, or type 'skip'.")
				return
			}
			text = email

			if h.emailLinkInitiator != nil {
				if err := h.emailLinkInitiator(ctx, msg.Conversation.UserID, msg.Conversation.IdentityID, msg.Conversation.ChatID, text); err != nil {
					log.Printf("runtime: agent %q: email link initiator: %v", hosted.Agent.ID, err)
					h.deliverBestEffort(ctx, hosted, msg.Conversation, "Something went wrong sending the verification email. Please try again.")
					return
				}
			}
			if h.emailLinkStateSetter != nil {
				_ = h.emailLinkStateSetter(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, "sent")
			}
			h.deliverBestEffort(ctx, hosted, msg.Conversation, "Check your inbox and click the link to verify your email address. Once verified your account will be linked across platforms.")
			return
		}

		// Auto-prompt removed — email linking is now user-initiated via /settings.
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
				var integrationName string
				if h.connectIntegrationNameLoader != nil {
					if name, err := h.connectIntegrationNameLoader(ctx, identityID); err == nil {
						integrationName = name
					}
				}
				text, buttonLabel := h.connectPrompt(ctx, hosted.Agent, hosted.Agent.LLMs, integrationName, "")
				var extra map[string]any
				if !isLocalhostURL(connectURL) {
					extra = map[string]any{
						"web_app_button": map[string]any{
							"text": buttonLabel,
							"url":  connectURL,
						},
					}
				}
				h.deliverBestEffort(ctx, hosted, msg.Conversation, text, extra)
				return
			}
			return
		}
	}

	// Pending instructions reply: user sent text in reply to a ForceReply /
	// text-based instructions prompt.
	instrKey := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID
	if ps, ok := h.pendingInstructions.Load(instrKey); ok {
		h.pendingInstructions.Delete(instrKey)
		state := ps.(pendingInstructionsState)
		if h.userAgentConfigSetter != nil {
			if err := h.userAgentConfigSetter(ctx, state.userID, state.agentID, msg.Text); err != nil {
				log.Printf("runtime: agent %q: save instructions user %s: %v", hosted.Agent.ID, state.userID, err)
				h.deliverBestEffort(ctx, hosted, msg.Conversation, "Sorry, I couldn't save your instructions. Please try again.")
				return
			}
		}
		h.deliverBestEffort(ctx, hosted, msg.Conversation, "Got it! I've saved your personal instructions and will keep them in mind going forward.")
		return
	}

	// Settings menu command: intercept before the LLM turn loop.
	if h.settingsFlow != nil && isSettingsCommand(msg.Text) {
		if executor, ok := h.executorForMessage(hosted, msg); ok {
			if provider, ok := executor.(domain.SettingsMenuProvider); ok {
				sCtx := ctx
				if identity, ok := hosted.Deps.IdentitiesByID[msg.Conversation.IdentityID]; ok {
					sCtx = domain.WithBotCredentialRef(sCtx, identity.CredentialRef)
				}
				if msg.Conversation.UserID != "" && h.userConnectionRefLoader != nil {
					sCtx = h.withUserConnectionRefs(sCtx, msg.Conversation.UserID)
				}
				h.settingsFlow.Open(sCtx, provider, hosted, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.UserID)
				return
			}
		}
	}

	// Custom bot commands: if the message is a registered command, replace the
	// text with the command's Message before running HandleTurn. This lets
	// creators map /help → "What can you do?" without any listener-side code.
	if cmdText := msg.Text; len(hosted.Agent.Commands) > 0 {
		if mapped := matchBotCommand(cmdText, hosted.Agent.Commands); mapped != "" {
			msg.Text = mapped
		}
	}

	// /instructions command: prompt the user to set their personal agent instructions.
	if isInstructionsCommand(msg.Text) && msg.Conversation.UserID != "" {
		if executor, ok := h.executorForMessage(hosted, msg); ok {
			if prompter, ok := executor.(domain.InstructionsPromptProvider); ok {
				var current string
				if h.userAgentConfigLoader != nil {
					if cfg, err := h.userAgentConfigLoader(ctx, msg.Conversation.UserID, hosted.Agent.ID); err == nil {
						current = cfg.Instructions
					}
				}
				wait, err := prompter.SendInstructionsPrompt(ctx, msg.Conversation.ChatID, current, "")
				if err != nil {
					log.Printf("runtime: agent %q: send instructions prompt: %v", hosted.Agent.ID, err)
					return
				}
				if wait {
					h.pendingInstructions.Store(instrKey, pendingInstructionsState{
						userID:  msg.Conversation.UserID,
						agentID: hosted.Agent.ID,
					})
				}
				return
			}
		}
	}

	// Carry the inbound message ID into the ConversationRef so
	// ConversationActionProvider implementations can react to the triggering
	// message without the model needing to know or supply the ID.
	if msg.MessageID != "" {
		msg.Conversation.MessageID = msg.MessageID
	}
	ctx = domain.WithOriginalMessage(ctx, msg.Text)
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
		if userID, err := h.userChannelResolver(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.SenderID); err == nil {
			msg.Conversation.UserID = userID
		}
	}

	if strings.HasPrefix(cq.Data, "kael_sm:") {
		executor, ok := h.executorForMessage(hosted, msg)
		if !ok {
			return
		}

		action := cq.Data[len("kael_sm:"):]

		if action == "instructions" {
			h.handleInstructionsCallback(ctx, hosted, msg, executor)
			return
		}

		if action == "link_account" {
			h.handleLinkAccountCallback(ctx, hosted, msg, executor)
			return
		}

		if h.settingsFlow == nil {
			return
		}
		provider, ok := executor.(domain.SettingsMenuProvider)
		if !ok {
			return
		}
		sCtx := ctx
		if identity, ok := hosted.Deps.IdentitiesByID[msg.Conversation.IdentityID]; ok {
			sCtx = domain.WithBotCredentialRef(sCtx, identity.CredentialRef)
		}
		if msg.Conversation.UserID != "" && h.userConnectionRefLoader != nil {
			sCtx = h.withUserConnectionRefs(sCtx, msg.Conversation.UserID)
		}
		h.settingsFlow.Handle(sCtx, provider, hosted,
			msg.Conversation.IdentityID, msg.Conversation.ChatID,
			cq.MessageID, msg.Conversation.UserID, action)

		// Dismiss the loading spinner on the client after the action completes.
		if ack, ok := executor.(domain.CallbackQueryAcknowledger); ok {
			go func() { _ = ack.AcknowledgeCallbackQuery(context.Background(), cq.QueryID) }()
		}
	}
}

// handleInstructionsCallback handles the kael_sm:instructions button tap.
// It loads the user's current instructions, calls SendInstructionsPrompt on
// the executor (opening a modal for Slack, sending ForceReply for Telegram),
// and tracks pending state when a text reply is expected.
func (h *Host) handleInstructionsCallback(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage, executor domain.Executor) {
	cq := msg.CallbackQuery

	prompter, ok := executor.(domain.InstructionsPromptProvider)
	if !ok {
		return
	}

	// Close the settings menu before opening the instructions prompt.
	if provider, ok := executor.(domain.SettingsMenuProvider); ok {
		if err := provider.DeleteSettingsMenu(ctx, msg.Conversation.ChatID, cq.MessageID); err != nil {
			log.Printf("runtime: instructions: delete settings menu: %v", err)
		}
	}

	var current string
	if h.userAgentConfigLoader != nil && msg.Conversation.UserID != "" {
		if cfg, err := h.userAgentConfigLoader(ctx, msg.Conversation.UserID, hosted.Agent.ID); err == nil {
			current = cfg.Instructions
		}
	}

	wait, err := prompter.SendInstructionsPrompt(ctx, msg.Conversation.ChatID, current, cq.TriggerID)
	if err != nil {
		log.Printf("runtime: instructions: send prompt: %v", err)
	}

	if wait && msg.Conversation.UserID != "" {
		key := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID
		h.pendingInstructions.Store(key, pendingInstructionsState{
			userID:  msg.Conversation.UserID,
			agentID: hosted.Agent.ID,
		})
	}

	if ack, ok := executor.(domain.CallbackQueryAcknowledger); ok {
		go func() { _ = ack.AcknowledgeCallbackQuery(context.Background(), cq.QueryID) }()
	}
}

// handleInstructionsSubmission handles an InstructionsSubmission message
// (from a Slack modal) by saving the text as the user's personal instructions.
func (h *Host) handleInstructionsSubmission(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	// Resolve user if not already set.
	if msg.Conversation.UserID == "" && h.userChannelResolver != nil {
		if userID, err := h.userChannelResolver(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, msg.Conversation.SenderID); err == nil {
			msg.Conversation.UserID = userID
		}
	}
	if msg.Conversation.UserID == "" {
		return
	}
	if h.userAgentConfigSetter == nil {
		return
	}
	text := msg.InstructionsSubmission.Text
	if err := h.userAgentConfigSetter(ctx, msg.Conversation.UserID, hosted.Agent.ID, text); err != nil {
		log.Printf("runtime: agent %q: save instructions modal submission user %s: %v", hosted.Agent.ID, msg.Conversation.UserID, err)
		h.deliverBestEffort(ctx, hosted, msg.Conversation, "Sorry, I couldn't save your instructions. Please try again.")
		return
	}
	h.deliverBestEffort(ctx, hosted, msg.Conversation, "Got it! I've saved your personal instructions and will keep them in mind going forward.")
}

// handleLinkAccountCallback handles the kael_sm:link_account button tap.
// It closes the settings menu, sets pending email link state, and prompts the
// user to type their email address. The existing pendingEmailLinks reply handler
// then processes their response.
func (h *Host) handleLinkAccountCallback(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage, executor domain.Executor) {
	cq := msg.CallbackQuery

	// Close the settings menu.
	if provider, ok := executor.(domain.SettingsMenuProvider); ok {
		if err := provider.DeleteSettingsMenu(ctx, msg.Conversation.ChatID, cq.MessageID); err != nil {
			log.Printf("runtime: link account: delete settings menu: %v", err)
		}
	}

	// Arm the pending email link state.
	emailKey := msg.Conversation.IdentityID + ":" + msg.Conversation.ChatID
	h.pendingEmailLinks.Store(emailKey, true)
	if h.emailLinkStateSetter != nil {
		_ = h.emailLinkStateSetter(ctx, msg.Conversation.IdentityID, msg.Conversation.ChatID, "awaiting")
	}

	h.deliverBestEffort(ctx, hosted, msg.Conversation, "Reply with your email address to link your account across platforms, or type 'skip' to cancel.")

	if ack, ok := executor.(domain.CallbackQueryAcknowledger); ok {
		go func() { _ = ack.AcknowledgeCallbackQuery(context.Background(), cq.QueryID) }()
	}
}

// isSettingsCommand reports whether text is a settings-menu trigger.
func isSettingsCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return t == "/settings" || t == "settings"
}

// isInstructionsCommand reports whether text is a personal-instructions trigger.
func isInstructionsCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return t == "/instructions" || t == "instructions"
}

// matchBotCommand checks whether text matches one of the agent's registered
// BotCommands (with or without a leading "/"). Returns the command's Message
// when there is a match and Message is non-empty, otherwise returns "".
func matchBotCommand(text string, commands []domain.BotCommand) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "/") {
		t = t[1:]
	}
	// Strip any parameters (e.g. "/help arg1" → "help")
	if i := strings.IndexByte(t, ' '); i >= 0 {
		t = t[:i]
	}
	t = strings.ToLower(t)
	for _, cmd := range commands {
		if strings.ToLower(cmd.Command) == t && cmd.Message != "" {
			return cmd.Message
		}
	}
	return ""
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

var emailRegexp = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// extractEmail pulls the first RFC-5321-ish email address out of arbitrary
// text. Users often type natural language ("my email is foo@bar.com") so we
// cannot assume the entire input is an address.
func extractEmail(text string) string {
	return emailRegexp.FindString(text)
}

func recoverFromPanic(agentID, where string) {
	if r := recover(); r != nil {
		log.Printf("runtime: agent %q: recovered panic in %s: %v", agentID, where, r)
	}
}
