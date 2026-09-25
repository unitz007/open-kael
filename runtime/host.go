// Package runtime is the framework's own runtime host — the piece that
// actually runs a domain.Agent against live traffic: turn handling
// (this file), inbound listening, cron, and webhook dispatch (see the
// other files in this package). Nothing here is provider-specific; a
// concrete Listener/Executor for Slack, Telegram, or anything else is an
// implementation detail that belongs to whoever embeds this framework, not
// to this package (see executors/ in this repo's own doc comments for that
// boundary).
package runtime

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

// AgentDeps is everything a HostedAgent needs beyond domain.Agent itself to
// actually run — the lookup maps domain.HydrateSkillTools needs, plus an
// optional Memory. Kept separate from domain.Agent because these are
// runtime wiring (in-memory maps today, a database-backed lookup later),
// not stored agent data.
type AgentDeps struct {
	ToolsByID        map[string]*domain.ToolDefinition
	IdentitiesByID   map[string]*domain.Identity
	IntegrationsByID map[string]*domain.Integration
	Executors        *domain.ExecutorRegistry
	// Memory is optional — nil means every turn starts cold, same as
	// domain.BindSkill's nested loop does today.
	Memory domain.Memory
}

// HostedAgent is one Agent as the runtime Host actually runs it.
type HostedAgent struct {
	Agent *domain.Agent
	Deps  AgentDeps
}

// Host runs a set of registered agents' turns, cron schedules, and event
// dispatch. The zero value is not ready to use — call NewHost.
type Host struct {
	mu     sync.RWMutex
	agents map[string]*HostedAgent

	// listenerCancels stores a cancel func for each running listener goroutine,
	// keyed by "agentID:identityID". Allows individual listeners to be stopped
	// when an identity is removed from an agent (see StopIdentityListeners).
	listenerCancels sync.Map

	// agentInboxes stores the message inbox per agentID so dynamic listeners
	// added via StartListenerForAgent can feed into the same channel as the
	// agent's existing processing goroutine.
	agentInboxes sync.Map

	// serveCtx is the context passed to ListenAndServe; stored so
	// StartListenerForAgent can start goroutines on the same lifecycle.
	serveCtx context.Context

	// serveWG tracks all goroutines started by ListenAndServe and by
	// StartListenerForAgent. A sentinel goroutine keeps it non-zero while
	// ctx is live, so dynamically added goroutines never race with Wait.
	serveWG sync.WaitGroup

	// bus dispatches named events to subscribed Skills — populated by
	// RegisterEventSources and fed by Emit for internal system events.
	bus *EventBus

	// llmFactory, when set, is called to build a live LLM slice for an
	// Agent being hydrated at MCP session time — so a freshly-created Agent
	// gets its LLMs without a server restart. Set via SetLLMFactory.
	llmFactory func(*domain.Agent) []domain.LLM

	// userChannelResolver maps a {identityID, channelRef} pair to the platform
	// UserID that registered that messenger address. Set via
	// SetUserChannelResolver; nil means user scoping is disabled.
	userChannelResolver func(ctx context.Context, identityID, channelRef string) (string, error)

	// userConnectionRefLoader loads a map of identityID → connectionRef for
	// a user's AppAuthorizations. Combined with userChannelResolver, this
	// scopes tool execution to the right per-user credentials each turn.
	userConnectionRefLoader func(ctx context.Context, userID string) (map[string]string, error)

	// eventActorRefLoader resolves an external actor identifier from an event
	// payload into a map of identityID → connectionRef for that actor's
	// AppAuthorizations. sourceIdentityID identifies which Identity received
	// the event (used to find the matching AppAuthorization by external user
	// ID); actorExternalID is the provider's own user identifier (e.g. a
	// GitHub login). Returns nil/empty when no matching user is found — the
	// skill then runs without per-user connection refs (bot-only).
	eventActorRefLoader func(ctx context.Context, sourceIdentityID, actorExternalID string) (map[string]string, error)

	// channelRedeemer, when set, is called on every inbound message before
	// HandleTurn. If the text contains a valid ChannelLinkCode (either as a
	// plain token or as a Telegram /start <code> deep link), the callback
	// saves the MessengerChannel, deletes the code, and returns the platform
	// UserID. domain.ErrNotFound means the text is not a code — fall through
	// to normal HandleTurn. Any other error is a redemption failure.
	channelRedeemer func(ctx context.Context, code, identityID, channelRef string) (userID string, err error)

	// autoProvisioner, when set, is called when an inbound message arrives from
	// an unrecognised channelRef. It creates a new platform User and a
	// MessengerChannel binding, returning the new user's ID so the turn can
	// proceed immediately without a link-code round-trip.
	autoProvisioner func(ctx context.Context, identityID, channelRef string) (userID string, err error)

	// setupChecker, when set, is called after userID is known. It returns the
	// identityIDs of the agent's integrations that the user hasn't yet
	// authorised. An empty slice means the user is fully set up.
	setupChecker func(ctx context.Context, userID string, agent *domain.Agent) (missingIdentityIDs []string, err error)

	// connectURLGenerator, when set, is called for each identity ID returned
	// by setupChecker. It returns the OAuth URL the user should visit to
	// authorise that integration, or an empty string when the integration
	// doesn't support a URL-based flow.
	connectURLGenerator func(ctx context.Context, userID, identityID string) (string, error)

	// pendingSetups tracks in-progress in-bot credential collection flows.
	// Key is "identityID:chatID"; value is *pendingCredential.
	pendingSetups sync.Map

	// credentialSaver, when set, is called after the user pastes back the OAuth
	// redirect URL containing the authorization code and signed state. It should
	// verify the state, exchange the code for tokens, and persist an AppAuthorization.
	credentialSaver func(ctx context.Context, userID, identityID, code, state string) error

	// linkCodeGenerator, when set, is called when an unlinked user messages the
	// bot. It creates and stores a short-lived ChannelLinkCode for the given
	// (identityID, channelRef) pair and returns the opaque code string. The host
	// embeds the code in a URL pointing at frontendURL so the user can link their
	// account with a single click.
	linkCodeGenerator func(ctx context.Context, identityID, channelRef string) (code string, err error)

	// frontendURL is the base URL of the creator/user-facing web app. When
	// set, unlinked users receive an onboarding message that includes this URL
	// so they know where to sign up and get their link code.
	frontendURL string

	// jev, when set, is used to route inbound messages to the correct skill
	// before the inner skill loop runs — replacing the outer NativeLoop's
	// LLM-based skill selection with a fast, type-safe Jev classifier.
	skillRouter SkillRouter
}

// SkillRouter picks the right skill for an inbound message. Any classifier
// — a remote API, a local model, a keyword matcher — can implement this;
// the concrete TypeSafe AI Jev implementation lives in the root jev package.
type SkillRouter interface {
	// PickSkill returns the name of the skill that should handle userText, and
	// the classifier's confidence (0–1). Returns ("", 0, nil) when no skill fits.
	PickSkill(ctx context.Context, userText string, skills []*domain.Skill) (skillName string, confidence float64, err error)
}

func NewHost() *Host {
	return &Host{agents: make(map[string]*HostedAgent), bus: newEventBus()}
}

// SetSkillRouter registers a skill router. When set, HandleTurn uses it to
// pick which skill should handle the user's message instead of running the
// outer NativeLoop. Falls back to NativeLoop when the router returns low
// confidence or no matching skill.
func (h *Host) SetSkillRouter(r SkillRouter) {
	h.skillRouter = r
}

// SetUserChannelResolver registers a function that maps {identityID, channelRef}
// to a platform UserID. When set, the host resolves the user on every inbound
// message and makes their connection refs available to the turn.
func (h *Host) SetUserChannelResolver(f func(ctx context.Context, identityID, channelRef string) (string, error)) {
	h.userChannelResolver = f
}

// SetUserConnectionRefLoader registers a function that returns a map of
// identityID → connectionRef for a user's AppAuthorizations. Used together
// with SetUserChannelResolver to scope tool execution to the right per-user
// credentials each turn.
func (h *Host) SetUserConnectionRefLoader(f func(ctx context.Context, userID string) (map[string]string, error)) {
	h.userConnectionRefLoader = f
}

// SetChannelRedeemer registers a function that validates and redeems a
// ChannelLinkCode. Called on every inbound message before HandleTurn —
// if the message text (or its /start parameter on Telegram) matches a
// stored code, the function saves the MessengerChannel, deletes the code,
// and returns the platform UserID so the host can emit user.channel.connected.
func (h *Host) SetChannelRedeemer(f func(ctx context.Context, code, identityID, channelRef string) (string, error)) {
	h.channelRedeemer = f
}

// SetFrontendURL sets the base URL of the web app. When set, unlinked users
// who message a bot receive an onboarding reply that includes this URL.
func (h *Host) SetFrontendURL(url string) { h.frontendURL = url }

// SetLinkCodeGenerator registers a function that creates and stores a
// short-lived ChannelLinkCode for the given (identityID, channelRef) pair
// and returns the opaque code string. Called when an unlinked user messages
// the bot so the host can embed a ready-to-use link in the onboarding reply.
func (h *Host) SetLinkCodeGenerator(f func(ctx context.Context, identityID, channelRef string) (string, error)) {
	h.linkCodeGenerator = f
}

// SetAutoProvisioner registers a function that creates a new User and
// MessengerChannel for a first-time bot user, returning the new user's ID.
// When set, first messages from unknown users immediately get a user identity
// instead of a link-code round-trip.
func (h *Host) SetAutoProvisioner(f func(ctx context.Context, identityID, channelRef string) (string, error)) {
	h.autoProvisioner = f
}

// SetSetupChecker registers a function that returns the identityIDs of an
// agent's integrations that the user hasn't yet authorised. Called on every
// turn when the user is known; if the returned list is non-empty the host
// prompts the user to connect before proceeding.
func (h *Host) SetSetupChecker(f func(ctx context.Context, userID string, agent *domain.Agent) ([]string, error)) {
	h.setupChecker = f
}

// SetConnectURLGenerator registers a function that generates the OAuth URL
// the user should visit to authorise a specific integration identity. Returns
// an empty string for identities that don't use a URL-based flow.
func (h *Host) SetConnectURLGenerator(f func(ctx context.Context, userID, identityID string) (string, error)) {
	h.connectURLGenerator = f
}

// SetCredentialSaver registers a function that is called after the user pastes
// the OAuth redirect URL back into the bot. It receives the authorization code
// and signed state extracted from that URL, and must verify the state, exchange
// the code, and persist an AppAuthorization for the given user and identity.
func (h *Host) SetCredentialSaver(f func(ctx context.Context, userID, identityID, code, state string) error) {
	h.credentialSaver = f
}

// SetEventActorRefLoader registers a function that resolves a webhook sender's
// external user ID (e.g. a GitHub login) into a map of identityID →
// connectionRef for that user's AppAuthorizations. When set, event-triggered
// Skills whose payload carries an attributable sender gain per-user connection
// refs — enabling them to act on the sender's behalf across providers (e.g.
// a GitHub webhook triggering a Slack DM using the sender's own Slack token).
func (h *Host) SetEventActorRefLoader(f func(ctx context.Context, sourceIdentityID, actorExternalID string) (map[string]string, error)) {
	h.eventActorRefLoader = f
}

// SetLLMFactory registers a function that builds the live LLM slice for an
// Agent from its stored LLMConfig. Called once at startup; used by
// RegisterMCP to wire LLMs into each per-session HostedAgent so multi-tool
// Skills have an LLM available even for Agents created after the server
// started.
func (h *Host) SetLLMFactory(factory func(*domain.Agent) []domain.LLM) {
	h.llmFactory = factory
}

// StopIdentityListeners cancels the listener goroutines for the given
// (agentID, identityID) pairs. Called when identities are removed from an
// agent via the API so the goroutines exit cleanly rather than continuing
// to poll a disconnected credential.
func (h *Host) StopIdentityListeners(agentID string, identityIDs []string) {
	for _, id := range identityIDs {
		key := agentID + ":" + id
		if cancel, ok := h.listenerCancels.LoadAndDelete(key); ok {
			cancel.(context.CancelFunc)()
		}
	}
}

// Register adds or replaces a HostedAgent. Safe to call while the host is
// already running Start (see listen.go) — later lookups always see the
// latest registration.
func (h *Host) Register(hosted *HostedAgent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents[hosted.Agent.ID] = hosted
}

// Get looks up a previously Register'd HostedAgent by Agent.ID.
func (h *Host) Get(agentID string) (*HostedAgent, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	hosted, ok := h.agents[agentID]
	return hosted, ok
}

// ReloadAgentSkills refreshes the Skills slice on a registered HostedAgent by
// calling loader, then replaces the in-memory agent. Safe to call at runtime —
// the next turn and the next onboarding message both see the updated skills.
func (h *Host) ReloadAgentSkills(ctx context.Context, agentID string, loader func(context.Context, string) (*domain.Agent, error)) {
	fresh, err := loader(ctx, agentID)
	if err != nil {
		log.Printf("runtime: ReloadAgentSkills agent %q: %v", agentID, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	hosted, ok := h.agents[agentID]
	if !ok {
		return
	}
	hosted.Agent.Skills = fresh.Skills
}

// buildSystemPrompt constructs the agent's system prompt dynamically from its
// name, description, and current skill list. This keeps the prompt in sync
// with the agent's live configuration — adding, editing, or removing a skill
// is reflected automatically on the next turn without any manual edit to an
// instructions field. If the agent has an explicit Instructions override it
// is appended after the generated block so operators can still inject custom
// behaviour without giving up the auto-generated foundation.
func buildSystemPrompt(agent *domain.Agent) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are %s.", agent.Name)
	if agent.Description != "" {
		fmt.Fprintf(&b, " %s", agent.Description)
	}

	if len(agent.Skills) > 0 {
		b.WriteString("\n\nYou have the following skills:\n")
		for _, s := range agent.Skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
		b.WriteString("\nOnly operate within these skills.")
	}

	if agent.Instructions != "" {
		fmt.Fprintf(&b, "\n\n%s", agent.Instructions)
	}

	return b.String()
}

// buildFinishAction is the top-level turn's own "I have my final answer"
// action — built fresh per call since its Spec carries no state. Mirrors
// domain.BindSkill's own local finish action, generalized to free-text
// content instead of a Skill's typed OutputSchema, the same way
// kael-platform's end_loop/final_message pairing works at the top level.
func buildFinishAction() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        domain.FinishActionName,
			Description: "Call this once you have your final answer for the user.",
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"content": {Type: domain.SchemaTypeString, Description: "Your final answer."},
				},
				Required: []string{"content"},
			},
		},
	}
}

// actionsFor assembles everything hosted's own top-level loop may call this
// turn: finish, every one of its own Skills (hydrated fresh — cheap, and
// picks up any Skill/Tool/Integration change registered since the last
// turn), and its delegate targets' Public Skills.
func actionsFor(ctx context.Context, hosted *HostedAgent) ([]*domain.BoundAction, error) {
	actions := []*domain.BoundAction{buildFinishAction()}

	for _, skill := range hosted.Agent.Skills {
		tools, err := domain.HydrateSkillTools(skill, hosted.Agent, hosted.Deps.ToolsByID, hosted.Deps.IdentitiesByID, hosted.Deps.IntegrationsByID, hosted.Deps.Executors)
		if err != nil {
			return nil, fmt.Errorf("hosting agent %q: skill %q: %w", hosted.Agent.ID, skill.ID, err)
		}
		actions = append(actions, domain.BindSkill(hosted.Agent, skill, tools))
	}

	if hosted.Agent.Directory != nil {
		actions = append(actions, hosted.Agent.Directory.PublicSkills(ctx)...)
	}

	return actions, nil
}

// resolveIdentityAndIntegration finds the Identity and Integration for a
// conversation's IdentityID. Falls back to searching by Provider when
// IdentityID is not set.
func resolveIdentityAndIntegration(hosted *HostedAgent, conv domain.ConversationRef) (*domain.Identity, *domain.Integration) {
	if conv.IdentityID != "" {
		if identity, ok := hosted.Deps.IdentitiesByID[conv.IdentityID]; ok {
			if integration, ok := hosted.Deps.IntegrationsByID[identity.IntegrationID]; ok {
				return identity, integration
			}
		}
	}
	// Fallback: find the first identity whose integration has this service
	for _, identity := range hosted.Deps.IdentitiesByID {
		if intg, ok := hosted.Deps.IntegrationsByID[identity.IntegrationID]; ok {
			if intg.Service == conv.Provider {
				return identity, intg
			}
		}
	}
	return nil, nil
}

// HandleTurn runs one full conversational turn for hosted — the runtime
// host's equivalent of kael-platform's Agent.RunLoop: load memory, build
// messages, run the agent's Loop over every action it can currently reach,
// persist new turns, and guarantee the user gets a reply even if the model
// never explicitly used a send action along the way.
func (h *Host) HandleTurn(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, userText string) (*domain.LoopResult, error) {
	// Inject user's per-identity connection refs into ctx when the user is
	// known — makes their AppAuthorization credentials available for tool
	// execution this turn via domain.ConnectionRefFromContext.
	if conv.UserID != "" && h.userConnectionRefLoader != nil {
		ctx = h.withUserConnectionRefs(ctx, conv.UserID)
	}
	ctx = domain.WithConversation(ctx, conv)
	if requester, ok := resolveApprovalRequester(hosted, conv); ok {
		ctx = domain.WithApprovalRequester(ctx, requester)
	}

	memKey := conv.Provider + ":" + conv.ChatID + ":" + conv.ThreadID
	var prior []domain.Message
	if hosted.Deps.Memory != nil {
		prior = hosted.Deps.Memory.History(ctx, memKey)
	}

	messages := make([]domain.Message, 0, len(prior)+2)
	messages = append(messages, domain.Message{Role: domain.RoleSystem, Content: buildSystemPrompt(hosted.Agent)})
	messages = append(messages, prior...)
	messages = append(messages, domain.Message{Role: domain.RoleUser, Content: userText})

	actions, err := actionsFor(ctx, hosted)
	if err != nil {
		return nil, err
	}

	var result *domain.LoopResult
	var final []domain.Message

	if h.skillRouter != nil && len(hosted.Agent.Skills) > 0 {
		result, final, err = h.routeWithJev(ctx, hosted, userText, messages, actions)
	} else {
		loop := hosted.Agent.Loop
		if loop == nil {
			loop = domain.NewNativeLoop(hosted.Agent.LLMs, hosted.Agent.MaxIterations)
		}
		result, final, err = loop.Run(ctx, messages, actions)
	}

	if hosted.Deps.Memory != nil && len(final) >= 1+len(prior) {
		hosted.Deps.Memory.Append(ctx, memKey, final[1+len(prior):]...)
	}

	if err != nil {
		h.deliverBestEffort(ctx, hosted, conv, "Sorry, I ran into an error and couldn't finish handling that. Please try again.")
		return result, err
	}

	content := result.Content
	switch {
	case content != "":
		// use as-is
	case result.Status == domain.LoopStatusComplete:
		content = "Done — the task completed, but I didn't leave a summary."
	default:
		content = "Sorry, I ran into an error and couldn't finish handling that. Please try again."
	}
	h.deliverBestEffort(ctx, hosted, conv, content)

	return result, nil
}

const jevConfidenceThreshold = 0.65

// routeWithJev uses the SkillRouter to pick the right skill for userText,
// then invokes it directly — skipping the outer NativeLoop's LLM-based
// selection. Falls back to NativeLoop on low confidence, no match, or error.
func (h *Host) routeWithJev(ctx context.Context, hosted *HostedAgent, userText string, messages []domain.Message, actions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	skillName, confidence, err := h.skillRouter.PickSkill(ctx, userText, hosted.Agent.Skills)
	if err != nil {
		log.Printf("skill-router: pick skill failed: %v — falling back to NativeLoop", err)
		return h.runNativeLoop(ctx, hosted, messages, actions)
	}

	log.Printf("skill-router: skill=%q confidence=%.2f", skillName, confidence)

	if skillName == "" || confidence < jevConfidenceThreshold {
		log.Printf("skill-router: low confidence or no match — falling back to NativeLoop")
		return h.runNativeLoop(ctx, hosted, messages, actions)
	}

	// Find the bound action for the chosen skill.
	for _, action := range actions {
		if action.Spec.Name != skillName {
			continue
		}
		output, err := action.Invoke(ctx, map[string]any{"message": userText})
		if err != nil {
			log.Printf("skill-router: invoke skill %q failed: %v", skillName, err)
			return &domain.LoopResult{Status: domain.LoopStatusError}, messages, err
		}
		content := domain.StringifyResult(output)
		return &domain.LoopResult{Status: domain.LoopStatusComplete, Content: content}, messages, nil
	}

	log.Printf("skill-router: skill %q not found in actions — falling back to NativeLoop", skillName)
	return h.runNativeLoop(ctx, hosted, messages, actions)
}

func (h *Host) runNativeLoop(ctx context.Context, hosted *HostedAgent, messages []domain.Message, actions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	loop := hosted.Agent.Loop
	if loop == nil {
		loop = domain.NewNativeLoop(hosted.Agent.LLMs, hosted.Agent.MaxIterations)
	}
	return loop.Run(ctx, messages, actions)
}

// withUserConnectionRefs loads the user's AppAuthorization connection refs
// and stores them in ctx via domain.WithConnectionRefs so BoundAction
// invocations can resolve the right per-user credential at call time.
func (h *Host) withUserConnectionRefs(ctx context.Context, userID string) context.Context {
	ctx = domain.WithUserID(ctx, userID)
	refs, err := h.userConnectionRefLoader(ctx, userID)
	if err != nil {
		log.Printf("runtime: load connection refs for user %q: %v", userID, err)
		return ctx
	}
	return domain.WithConnectionRefs(ctx, refs)
}

// onboardingMessage returns the intro text and link URL for an unlinked user's
// first message. When the agent has LLMs configured, the greeting body is
// phrased by the LLM; otherwise a plain template is used as fallback.
// linkURL is non-empty when the URL is suitable for an inline keyboard button
// (non-localhost); otherwise the URL is embedded in the text so local
// development still works.
func (h *Host) onboardingMessage(ctx context.Context, agent *domain.Agent, llms []domain.LLM, linkCode string) (text, linkURL string) {
	var publicSkills []*domain.Skill
	for _, s := range agent.Skills {
		if s.Visibility == domain.SkillPublic {
			publicSkills = append(publicSkills, s)
		}
	}

	body := h.greetingBody(ctx, agent, publicSkills, llms)

	var b strings.Builder
	b.WriteString(body)

	if linkCode != "" && h.frontendURL != "" {
		url := h.frontendURL + "/link?code=" + linkCode
		if isLocalhostURL(url) {
			b.WriteString("\n\nTo get started, connect your account:\n\n" + url)
		} else {
			linkURL = url
			b.WriteString("\n\nTap the button below to connect your account and get started.")
		}
		text = b.String()
		return
	}
	if h.frontendURL != "" {
		b.WriteString("\n\nTo chat with me, sign up or log in at " + h.frontendURL + " to connect your account.")
	} else {
		b.WriteString("\n\nTo chat with me you'll need to link your account first. Ask your platform administrator for the sign-up link.")
	}
	text = b.String()
	return
}

// greetingBody produces the conversational part of the onboarding message —
// the agent intro and capability list — phrased by the LLM when one is
// available, or by a plain template fallback.
func (h *Host) greetingBody(ctx context.Context, agent *domain.Agent, publicSkills []*domain.Skill, llms []domain.LLM) string {
	if len(llms) > 0 {
		var prompt strings.Builder
		prompt.WriteString("You are " + agent.Name + ".")
		if agent.Description != "" {
			prompt.WriteString(" " + agent.Description)
		}
		if len(publicSkills) > 0 {
			prompt.WriteString("\n\nYou can help users with:")
			for _, s := range publicSkills {
				prompt.WriteString("\n- " + skillDisplayName(s.Name))
				if s.Description != "" {
					prompt.WriteString(": " + s.Description)
				}
			}
		}
		if len(publicSkills) > 0 {
			prompt.WriteString("\n\nWrite a short, friendly greeting introducing yourself and what you can do. Plain text only — no markdown, no bullet points, no lists. Two to three sentences maximum.")
		} else {
			prompt.WriteString("\n\nWrite a short, friendly greeting introducing yourself. Do not mention or invent any specific capabilities — just say hello and introduce who you are. Plain text only — no markdown. Two sentences maximum.")
		}

		msgs := []domain.Message{{Role: domain.RoleUser, Content: prompt.String()}}
		if resp, err := llms[0].Call(ctx, msgs, nil); err == nil && resp.Content != "" {
			return resp.Content
		} else if err != nil {
			log.Printf("runtime: onboarding LLM greeting failed for agent %q: %v", agent.ID, err)
		} else {
			log.Printf("runtime: onboarding LLM greeting failed for agent %q: empty response", agent.ID)
		}
	}

	// Template fallback.
	var b strings.Builder
	b.WriteString("👋 Hi! I'm " + agent.Name + ".")
	if agent.Description != "" {
		b.WriteString("\n\n" + agent.Description)
	}
	if len(publicSkills) > 0 {
		b.WriteString("\n\nHere's what I can help you with:")
		for _, s := range publicSkills {
			b.WriteString("\n• " + skillDisplayName(s.Name))
			if s.Description != "" {
				b.WriteString(" — " + s.Description)
			}
		}
	}
	return b.String()
}

func isLocalhostURL(u string) bool {
	return strings.Contains(u, "localhost") || strings.Contains(u, "127.0.0.1")
}

func skillDisplayName(name string) string {
	words := strings.Split(name, "_")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// deliverBestEffort sends text back to conv via the Identity that received the
// inbound message. Resolves identity from conv.IdentityID (precise) or falls
// back to searching by provider. Failure is logged, not returned — a delivery
// failure shouldn't fail a turn that already completed. Optional extra maps are
// merged into the input so callers can add provider-specific fields (e.g. a
// Telegram inline keyboard button) without changing the signature for all callers.
func (h *Host) deliverBestEffort(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, text string, extra ...map[string]any) {
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		log.Printf("runtime: agent %q: no integration for provider %q, cannot deliver reply", hosted.Agent.ID, conv.Provider)
		return
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		log.Printf("runtime: agent %q: no executor for service %q, cannot deliver reply", hosted.Agent.ID, integration.Service)
		return
	}
	var connectionRef string
	if identity != nil {
		connectionRef, _ = domain.ConnectionRefFromContext(ctx, identity.ID)
	}
	input := map[string]any{"recipient": conv.ChatID, "text": text}
	for _, e := range extra {
		for k, v := range e {
			input[k] = v
		}
	}
	if _, err := executor.Execute(ctx, identity, connectionRef, domain.ActionSendMessage, input); err != nil {
		log.Printf("runtime: agent %q: failed to deliver reply: %v", hosted.Agent.ID, err)
	}
}
