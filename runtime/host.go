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
	"time"

	"github.com/unitz007/open-kael/domain"
)

// pendingAuthTurn holds the state needed to replay a turn that was interrupted
// by the auth gate — the original user message, conversation, and agent.
type pendingAuthTurn struct {
	conv        domain.ConversationRef
	agentID     string
	userMessage string
}

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
	// UserID that registered that messenger address. senderID is the stable
	// platform user identity (e.g. Slack user ID); may be empty. Set via
	// SetUserChannelResolver; nil means user scoping is disabled.
	userChannelResolver func(ctx context.Context, identityID, channelRef, senderID string) (string, error)

	// userConnectionRefLoader loads a map of identityID → connectionRef for
	// a user's AppAuthorizations. Combined with userChannelResolver, this
	// scopes tool execution to the right per-user credentials each turn.
	userConnectionRefLoader func(ctx context.Context, userID string) (map[string]string, error)

	// userToolApprovalLoader loads the set of tool IDs the user has configured
	// to require approval, as a map[toolID]bool. Injected into ctx via
	// domain.WithUserToolApprovals each turn so withUserApprovalGate can gate
	// those tools even when their ToolDefinition.RequiresApproval is false.
	userToolApprovalLoader func(ctx context.Context, userID string) (map[string]bool, error)

	// userToolApprovalSetter persists a user's approval preference for one tool.
	// When set, configure_tool_approval is injected into every user turn so the
	// user can manage their approval gates conversationally through the messenger.
	userToolApprovalSetter func(ctx context.Context, userID, toolID string, requires bool) error

	// settingsFlow, when non-nil, handles /settings commands intercepted before
	// HandleTurn and renders the button-driven settings menu through the
	// messenger's SettingsMenuProvider. Initialised automatically when both
	// userToolApprovalLoader and userToolApprovalSetter are set.
	settingsFlow *SettingsFlow

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
	// proceed immediately without a link-code round-trip. senderID is the
	// stable platform user identity (e.g. Slack user ID) — may be empty for
	// platforms that don't distinguish sender from channel.
	autoProvisioner func(ctx context.Context, identityID, channelRef, senderID string) (userID string, err error)

	// setupChecker, when set, is called after userID is known. It returns the
	// identityIDs of the agent's integrations that the user hasn't yet
	// authorised. An empty slice means the user is fully set up.
	setupChecker func(ctx context.Context, userID string, agent *domain.Agent) (missingIdentityIDs []string, err error)

	// connectURLGenerator, when set, is called for each identity ID returned
	// by setupChecker. It returns the OAuth URL the user should visit to
	// authorise that integration, or an empty string when the integration
	// doesn't support a URL-based flow.
	connectURLGenerator func(ctx context.Context, userID, identityID string) (string, error)

	// connectIntegrationNameLoader, when set, returns the human-readable name
	// of the integration for the given identityID (e.g. "Fantasy Premier League").
	// Used to generate LLM-phrased connect prompts instead of hardcoded text.
	connectIntegrationNameLoader func(ctx context.Context, identityID string) (name string, err error)

	// userChannelsByIdentityLoader, when set, returns all MessengerChannel
	// channel refs for the given (userID, identityID) pair. Used by
	// OnIntegrationConnected to deliver post-connection messages.
	userChannelsByIdentityLoader func(ctx context.Context, userID, identityID string) (channelRefs []string, err error)

	// pendingAuthTurns tracks turns that were interrupted by the auth gate.
	// Key is "userID:identityID"; value is pendingAuthTurn.
	// When OnIntegrationConnected fires, the stored turn is replayed in a
	// fresh goroutine so the agent can continue where it left off.
	pendingAuthTurns sync.Map

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

	// miniAppBaseURL is the base URL at which the Telegram Mini App is served
	// (e.g. "https://myapp.fly.dev"). When set, the /settings command sends a
	// web_app button pointing to <miniAppBaseURL>/miniapp/settings?agent=<agentID>
	// instead of the inline-keyboard SettingsFlow.
	miniAppBaseURL string

	// jev, when set, is used to route inbound messages to the correct skill
	// before the inner skill loop runs — replacing the outer NativeLoop's
	// LLM-based skill selection with a fast, type-safe Jev classifier.
	skillRouter SkillRouter

	// userAgentConfigLoader loads a user's personal instructions for one Agent.
	// When set, the instructions are injected as a <user_instructions> block in
	// the system prompt each turn. ErrNotFound is silently ignored (no config yet).
	userAgentConfigLoader func(ctx context.Context, userID, agentID string) (*domain.UserAgentConfig, error)

	// onboardingChecker reports whether the user has already completed the
	// onboarding intro. userID is always the resolved platform user; identityID
	// and channelRef are provided for implementations that need channel state.
	onboardingChecker func(ctx context.Context, userID, identityID, channelRef string) (onboarded bool, err error)

	// onboardingCompleter saves the user's intro text as their personal agent
	// instructions (user_agent_configs) and stamps onboarded_at on the
	// messenger_channel. Called after the user types their onboarding reply.
	onboardingCompleter func(ctx context.Context, identityID, channelRef, agentID, userID, instructions string) error

	// pendingOnboardings tracks channels that have received the onboarding
	// prompt and are waiting for the user's intro reply. Key is "identityID:chatID".
	pendingOnboardings sync.Map

	// onboardingPromptedChecker reports whether the onboarding prompt was sent
	// but the user hasn't replied yet. userID is provided so implementations
	// can short-circuit when the user is already onboarded on another channel.
	onboardingPromptedChecker func(ctx context.Context, userID, identityID, channelRef string) (bool, error)

	// onboardingPromptedMarker persists the fact that the onboarding prompt was
	// sent, so pending state survives restarts.
	onboardingPromptedMarker func(ctx context.Context, identityID, channelRef string) error

	// emailLinkChecker reports whether the user already has a verified email.
	emailLinkChecker func(ctx context.Context, userID string) (bool, error)

	// emailLinkStateGetter returns the current email_link_state for a channel.
	emailLinkStateGetter func(ctx context.Context, identityID, channelRef string) (string, error)

	// emailLinkStateSetter persists email_link_state changes so they survive restarts.
	emailLinkStateSetter func(ctx context.Context, identityID, channelRef, state string) error

	// emailLinkInitiator validates the email, creates a verification record, and
	// sends the verification email. Called when the user provides their email address.
	emailLinkInitiator func(ctx context.Context, userID, identityID, channelRef, email string) error

	// pendingEmailLinks tracks channels that have been prompted for an email and
	// are waiting for the user's reply. Key is "identityID:chatID".
	pendingEmailLinks sync.Map

	// userAgentConfigSetter persists a user's personal instructions for one
	// Agent. When set, the host saves the user's reply after SendInstructionsPrompt.
	userAgentConfigSetter func(ctx context.Context, userID, agentID, instructions string) error

	// pendingInstructions tracks channels that have received a text-based
	// instructions prompt (ForceReply or text fallback) and are waiting for
	// the user's reply. Key is "identityID:chatID"; value is pendingInstructionsState.
	pendingInstructions sync.Map

	// appInstructionsLoader returns per-app instructions for (userID, integrationID).
	appInstructionsLoader func(ctx context.Context, userID, integrationID string) (string, error)
	// appInstructionsSetter persists per-app instructions; empty text removes them.
	appInstructionsSetter func(ctx context.Context, userID, integrationID, text string) error

	// pendingAppInstructions tracks channels awaiting a per-app instructions
	// reply. Key is "identityID:chatID"; value is pendingAppInstructionsState.
	pendingAppInstructions sync.Map

	// userProfileLoader loads the user's general learned profile (user_profiles).
	userProfileLoader func(ctx context.Context, userID string) (*domain.UserProfile, error)
	// userProfileSetter upserts the user's general learned profile.
	userProfileSetter func(ctx context.Context, userID, notes string) error

	// userIntegrationNotesLoader loads all integration-specific notes for a user
	// as a map of integrationID → notes.
	userIntegrationNotesLoader func(ctx context.Context, userID string) (map[string]string, error)
	// userIntegrationNotesSetter upserts notes for one (user, integration) pair.
	userIntegrationNotesSetter func(ctx context.Context, userID, integrationID, notes string) error

	// profileExtractor, when set, is called in a background goroutine after each
	// substantial turn. It receives the exchange plus the current profile and
	// integration notes, and returns what (if anything) should be updated.
	profileExtractor func(ctx context.Context, userID, agentID string, input ProfileExtractInput) (*ProfileExtractResult, error)
}

// ProfileExtractInput groups the inputs passed to the profile extractor hook.
type ProfileExtractInput struct {
	UserMessage             string
	AgentResponse           string
	CurrentProfile          string            // from user_profiles
	IntegrationIDs          []string          // connected integrations to extract notes for
	IntegrationNames        map[string]string // integrationID → display name
	CurrentIntegrationNotes map[string]string // integrationID → current notes
}

// ProfileExtractResult groups what the profile extractor wants to save.
// Empty strings mean "no change" for that field; absent map keys mean no change
// for that integration.
type ProfileExtractResult struct {
	UpdatedProfile string            // empty = no change
	UpdatedNotes   map[string]string // integrationID → updated notes (omit if unchanged)
}

// SkillRouter picks the right skill(s) for an inbound message. Any classifier
// — a remote API, a local model, a keyword matcher — can implement this;
// the concrete TypeSafe AI Jev implementation lives in the root jev package.
type SkillRouter interface {
	// PickSkill returns the single best-matching skill name, the classifier's
	// confidence (0–1), and the user's intent. Returns ("", 0, "recommend", nil)
	// when no skill fits. Used by query_skill for single-skill delegation.
	PickSkill(ctx context.Context, userText string, skills []*domain.Skill) (skillName string, confidence float64, intent string, err error)

	// PickSkills returns all skills that apply to userText and the user's
	// intent. An empty slice means no skill matched. Used by the main routing
	// path to load and merge tools from multiple skills into one outer loop.
	PickSkills(ctx context.Context, userText string, skills []*domain.Skill) (skillNames []string, intent string, err error)
}

// ScopeChecker is an optional extension of SkillRouter. When the configured
// router also implements ScopeChecker, agents that have no skills use it to
// gate every inbound message against the agent's description before running
// the NativeLoop — so a skill-less agent still declines out-of-scope questions.
type ScopeChecker interface {
	// IsInScope returns true when userText falls within the agent's described
	// expertise. agentContext is the agent's description or a short summary of
	// its purpose, used as the classification context.
	IsInScope(ctx context.Context, userText, agentContext string) (bool, error)
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
// message and makes their connection refs available to the turn. senderID is
// the stable platform user identity (e.g. Slack user ID) — use it to look up
// or merge a user when the same person messages from multiple channels.
func (h *Host) SetUserChannelResolver(f func(ctx context.Context, identityID, channelRef, senderID string) (string, error)) {
	h.userChannelResolver = f
}

// SetUserConnectionRefLoader registers a function that returns a map of
// identityID → connectionRef for a user's AppAuthorizations. Used together
// with SetUserChannelResolver to scope tool execution to the right per-user
// credentials each turn.
func (h *Host) SetUserConnectionRefLoader(f func(ctx context.Context, userID string) (map[string]string, error)) {
	h.userConnectionRefLoader = f
}

// SetUserToolApprovalLoader registers a function that returns the set of tool
// IDs a user has configured to require approval. When set, the host loads this
// per turn and injects it into context via domain.WithUserToolApprovals so the
// user-level approval gate fires for those tools even when their
// ToolDefinition.RequiresApproval is false.
func (h *Host) SetUserToolApprovalLoader(f func(ctx context.Context, userID string) (map[string]bool, error)) {
	h.userToolApprovalLoader = f
	h.maybeInitSettingsFlow()
}

// SetUserToolApprovalSetter registers a function that persists a user's
// approval preference for one tool. When set, a configure_tool_approval action
// is injected into every user turn so they can manage approval gates
// conversationally through the messenger without touching a web UI.
func (h *Host) SetUserToolApprovalSetter(f func(ctx context.Context, userID, toolID string, requires bool) error) {
	h.userToolApprovalSetter = f
	h.maybeInitSettingsFlow()
}

// maybeInitSettingsFlow creates the SettingsFlow once both the approval loader
// and setter are registered. Safe to call multiple times — only creates once.
func (h *Host) maybeInitSettingsFlow() {
	if h.userToolApprovalLoader != nil && h.userToolApprovalSetter != nil && h.settingsFlow == nil {
		flow := newSettingsFlow(h.userToolApprovalLoader, h.userToolApprovalSetter)
		flow.checkLinkedEmail = h.emailLinkChecker
		flow.getAppInstructions = h.appInstructionsLoader
		flow.setAppInstructions = h.appInstructionsSetter
		h.settingsFlow = flow
	}
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

// SetMiniAppBaseURL sets the base URL at which the Telegram Mini App is hosted.
// When set, the /settings command sends a web_app button to the mini app instead
// of falling back to the inline-keyboard SettingsFlow.
func (h *Host) SetMiniAppBaseURL(url string) { h.miniAppBaseURL = url }

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
// instead of a link-code round-trip. senderID is the stable platform user
// identity (e.g. Slack user ID) — may be empty for platforms where the
// channel and user are the same (e.g. Telegram DMs).
func (h *Host) SetAutoProvisioner(f func(ctx context.Context, identityID, channelRef, senderID string) (string, error)) {
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

// SetUserAgentConfigLoader registers a function that loads a user's personal
// instructions for one Agent. When set, the instructions are injected as a
// <user_instructions> block into the system prompt on every turn so the agent
// can personalise its responses to that user.
func (h *Host) SetUserAgentConfigLoader(f func(ctx context.Context, userID, agentID string) (*domain.UserAgentConfig, error)) {
	h.userAgentConfigLoader = f
}

// SetUserAgentConfigSetter registers a function that persists a user's personal
// instructions for one Agent. When set, the host saves the reply text after
// SendInstructionsPrompt as the user's new instructions.
func (h *Host) SetUserAgentConfigSetter(f func(ctx context.Context, userID, agentID, instructions string) error) {
	h.userAgentConfigSetter = f
}

// SetAppInstructionsLoader registers a function that loads per-app instructions
// for (userID, integrationID). When set, the instructions sub-screen in the
// app settings menu shows the current text.
func (h *Host) SetAppInstructionsLoader(f func(ctx context.Context, userID, integrationID string) (string, error)) {
	h.appInstructionsLoader = f
	if h.settingsFlow != nil {
		h.settingsFlow.getAppInstructions = f
	}
}

// SetAppInstructionsSetter registers a function that persists per-app
// instructions for (userID, integrationID). Passing an empty string removes
// the stored instructions. When set, the host saves the user's reply after a
// per-app instructions prompt, and the Clear button removes existing text.
func (h *Host) SetAppInstructionsSetter(f func(ctx context.Context, userID, integrationID, text string) error) {
	h.appInstructionsSetter = f
	if h.settingsFlow != nil {
		h.settingsFlow.setAppInstructions = f
	}
}

// SetUserProfileLoader registers a function that loads the user's general
// learned profile (user_profiles table, keyed by user_id only).
func (h *Host) SetUserProfileLoader(f func(ctx context.Context, userID string) (*domain.UserProfile, error)) {
	h.userProfileLoader = f
}

// SetUserProfileSetter registers a function that upserts the user's general
// learned profile.
func (h *Host) SetUserProfileSetter(f func(ctx context.Context, userID, notes string) error) {
	h.userProfileSetter = f
}

// SetUserIntegrationNotesLoader registers a function that loads all
// integration-specific notes for a user as integrationID → notes.
func (h *Host) SetUserIntegrationNotesLoader(f func(ctx context.Context, userID string) (map[string]string, error)) {
	h.userIntegrationNotesLoader = f
}

// SetUserIntegrationNotesSetter registers a function that upserts notes for
// one (user, integration) pair.
func (h *Host) SetUserIntegrationNotesSetter(f func(ctx context.Context, userID, integrationID, notes string) error) {
	h.userIntegrationNotesSetter = f
}

// SetProfileExtractor registers a function that extracts new user facts from a
// completed turn. It runs in a background goroutine after each turn whose user
// message is substantial enough (>= 8 words) to plausibly contain personal
// information. Returns a ProfileExtractResult with empty strings / absent map
// keys meaning "no change". Only fires when userProfileSetter is also set.
func (h *Host) SetProfileExtractor(f func(ctx context.Context, userID, agentID string, input ProfileExtractInput) (*ProfileExtractResult, error)) {
	h.profileExtractor = f
}

// ReloadAgentCommands replaces the Commands slice on a registered HostedAgent.
// Called by closed-kael's API server hook when PUT /agents/{id}/commands saves
// a new command list — ensures the live runtime sees the update without a restart.
func (h *Host) ReloadAgentCommands(agentID string, commands []domain.BotCommand) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hosted, ok := h.agents[agentID]
	if !ok {
		return
	}
	hosted.Agent.Commands = commands
}

// SetOnboardingFlow registers the two callbacks that drive the first-message
// onboarding flow: checker reports whether the user has already completed
// their intro; completer saves the intro text and marks the channel onboarded.
// When both are set, the host intercepts the first message on any unboarded
// channel and prompts the user to introduce themselves before the normal turn.
func (h *Host) SetOnboardingFlow(
	checker func(ctx context.Context, userID, identityID, channelRef string) (bool, error),
	completer func(ctx context.Context, identityID, channelRef, agentID, userID, instructions string) error,
) {
	h.onboardingChecker = checker
	h.onboardingCompleter = completer
}

// SetOnboardingPendingCallbacks registers callbacks that persist the
// "onboarding prompt sent, awaiting reply" state to the database. When set,
// the pending state survives server restarts: checker returns true when the
// prompt was sent but onboarding is not yet complete; marker stamps that state.
func (h *Host) SetOnboardingPendingCallbacks(
	checker func(ctx context.Context, userID, identityID, channelRef string) (bool, error),
	marker func(ctx context.Context, identityID, channelRef string) error,
) {
	h.onboardingPromptedChecker = checker
	h.onboardingPromptedMarker = marker
}

// SetEmailLinkFlow registers the four callbacks that drive the optional
// in-chat email-linking flow. When set, the host prompts users to share their
// email address after onboarding completes; a verified email becomes the
// cross-platform identifier so the same user on Slack and Telegram shares one
// conversation history.
func (h *Host) SetEmailLinkFlow(
	checker func(ctx context.Context, userID string) (bool, error),
	stateGetter func(ctx context.Context, identityID, channelRef string) (string, error),
	stateSetter func(ctx context.Context, identityID, channelRef, state string) error,
	initiator func(ctx context.Context, userID, identityID, channelRef, email string) error,
) {
	h.emailLinkChecker = checker
	h.emailLinkStateGetter = stateGetter
	h.emailLinkStateSetter = stateSetter
	h.emailLinkInitiator = initiator
	if h.settingsFlow != nil {
		h.settingsFlow.checkLinkedEmail = checker
	}
}

// SetConnectIntegrationNameLoader registers a callback that returns the
// human-readable name of the integration for a given identityID. When set,
// the host uses the LLM to generate a natural connect prompt instead of the
// hardcoded fallback text.
func (h *Host) SetConnectIntegrationNameLoader(f func(ctx context.Context, identityID string) (string, error)) {
	h.connectIntegrationNameLoader = f
}

// SetUserChannelsByIdentityLoader registers a callback that returns the
// channel refs for a given (userID, identityID) pair. Required for
// OnIntegrationConnected to know which channels to deliver the message to.
func (h *Host) SetUserChannelsByIdentityLoader(f func(ctx context.Context, userID, identityID string) ([]string, error)) {
	h.userChannelsByIdentityLoader = f
}

// OnIntegrationConnected is called after a user successfully connects an
// integration. If the auth gate stored a pending turn for this user+identity,
// it replays the turn in a fresh goroutine so the agent continues where it left
// off. Otherwise it finds the user's channels, generates an LLM confirmation
// message, and delivers it. Safe to call in a goroutine.
func (h *Host) OnIntegrationConnected(ctx context.Context, userID, identityID string) {
	// Replay any turn that was interrupted by the auth gate for this user+identity.
	key := userID + ":" + identityID
	log.Printf("runtime: OnIntegrationConnected: user=%q identity=%q key=%q", userID, identityID, key)
	if val, ok := h.pendingAuthTurns.LoadAndDelete(key); ok {
		pending := val.(pendingAuthTurn)
		log.Printf("runtime: OnIntegrationConnected: found pending turn agent=%q message=%q — replaying", pending.agentID, pending.userMessage)
		h.mu.RLock()
		hosted := h.agents[pending.agentID]
		h.mu.RUnlock()
		if hosted != nil {
			replayCtx := domain.WithOriginalMessage(context.Background(), pending.userMessage)
			go func() {
				if _, err := h.HandleTurn(replayCtx, hosted, pending.conv, pending.userMessage); err != nil {
					log.Printf("runtime: replay turn after auth for user %q identity %q: %v", userID, identityID, err)
				}
			}()
		}
		return
	}
	log.Printf("runtime: OnIntegrationConnected: no pending turn for key=%q — sending connected message", key)

	if h.userChannelsByIdentityLoader == nil {
		return
	}
	channelRefs, err := h.userChannelsByIdentityLoader(ctx, userID, identityID)
	if err != nil || len(channelRefs) == 0 {
		return
	}
	var integrationName string
	if h.connectIntegrationNameLoader != nil {
		if name, err := h.connectIntegrationNameLoader(ctx, identityID); err == nil {
			integrationName = name
		}
	}
	// Find a hosted agent that has this identity so we can use its LLM.
	h.mu.RLock()
	var hosted *HostedAgent
	for _, ha := range h.agents {
		for _, id := range ha.Agent.IdentityIDs {
			if id == identityID {
				hosted = ha
				break
			}
		}
		if hosted != nil {
			break
		}
	}
	h.mu.RUnlock()
	if hosted == nil {
		return
	}
	text := h.connectedMessage(ctx, hosted.Agent, hosted.Agent.LLMs, integrationName)
	conv := domain.ConversationRef{IdentityID: identityID}
	for _, ref := range channelRefs {
		conv.ChatID = ref
		h.deliverBestEffort(ctx, hosted, conv, text)
	}
}

// connectedMessage generates a confirmation that the user has connected an
// integration, using the LLM when available.
func (h *Host) connectedMessage(ctx context.Context, agent *domain.Agent, llms []domain.LLM, integrationName string) string {
	if len(llms) > 0 {
		what := integrationName
		if what == "" {
			what = "the integration"
		}
		prompt := fmt.Sprintf(
			"You are %s.", agent.Name,
		)
		if agent.Description != "" {
			prompt += " " + agent.Description
		}
		prompt += fmt.Sprintf(
			"\n\nThe user has just successfully connected their %s account. "+
				"Write a short, warm confirmation (1-2 sentences) and invite them to try it out. "+
				"Plain text only — no markdown.",
			what,
		)
		msgs := []domain.Message{{Role: domain.RoleUser, Content: prompt}}
		if resp, err := llms[0].Call(ctx, msgs, nil); err == nil && resp.Content != "" {
			return resp.Content
		} else if err != nil {
			log.Printf("runtime: connected message LLM failed for agent %q: %v", agent.ID, err)
		}
	}
	if integrationName != "" {
		return "Your " + integrationName + " account is now connected! Feel free to ask me anything."
	}
	return "You're all connected! Feel free to ask me anything."
}

// connectPrompt generates a friendly message asking the user to connect their
// integration account, using the LLM when one is available.
// toolName is the name of the tool that triggered the auth gate — when provided
// the LLM prompt references the specific action so the message is contextual
// rather than generic.
// Returns (messageText, buttonLabel).
func (h *Host) connectPrompt(ctx context.Context, agent *domain.Agent, llms []domain.LLM, integrationName, toolName string) (text, buttonLabel string) {
	buttonLabel = "Connect " + integrationName + " account"
	if len(llms) > 0 && integrationName != "" {
		prompt := fmt.Sprintf("You are %s.", agent.Name)
		if agent.Description != "" {
			prompt += " " + agent.Description
		}
		if toolName != "" {
			prompt += fmt.Sprintf(
				"\n\nThe user just tried to use the \"%s\" tool, which requires their %s account. "+
					"Write a short, friendly message (1-2 sentences) explaining what they were trying to do and asking them to tap the button below to connect their %s account. "+
					"Plain text only — no markdown.",
				toolName, integrationName, integrationName,
			)
		} else {
			prompt += fmt.Sprintf(
				"\n\nThe user needs to connect their %s account to continue. "+
					"Write a short, friendly message (1-2 sentences) asking them to tap the button below to connect it. "+
					"Plain text only — no markdown.",
				integrationName,
			)
		}
		msgs := []domain.Message{{Role: domain.RoleUser, Content: prompt}}
		if resp, err := llms[0].Call(ctx, msgs, nil); err == nil && resp.Content != "" {
			return resp.Content, buttonLabel
		} else if err != nil {
			log.Printf("runtime: connect prompt LLM failed for agent %q: %v", agent.ID, err)
		}
	}
	fallback := "To use all features, tap below to connect your " + integrationName + " account."
	if integrationName == "" {
		fallback = "To continue, tap below to connect the required account."
	}
	return fallback, buttonLabel
}

// onboardingAck produces a warm acknowledgement of the user's intro text,
// phrased by the LLM when one is available, with a plain fallback.
func (h *Host) onboardingAck(ctx context.Context, agent *domain.Agent, llms []domain.LLM, userIntro string) string {
	if len(llms) > 0 && strings.TrimSpace(userIntro) != "" {
		prompt := fmt.Sprintf(
			"The user just introduced themselves: %q\n\n"+
				"Acknowledge what they shared in one warm, natural sentence, then invite them to ask their first question. "+
				"Plain text only — no markdown.",
			userIntro,
		)
		msgs := []domain.Message{{Role: domain.RoleUser, Content: prompt}}
		if resp, err := llms[0].Call(ctx, msgs, nil); err == nil && resp.Content != "" {
			return resp.Content
		} else if err != nil {
			log.Printf("runtime: onboarding ack LLM failed for agent %q: %v", agent.ID, err)
		}
	}
	return "Thanks for sharing that! What can I help you with?"
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

// ReloadAgent refreshes the mutable fields of a registered HostedAgent
// (Skills, Greeting, Description, Instructions) by calling loader. Safe to
// call at runtime — the next turn and the next onboarding message both see the
// updated values.
func (h *Host) ReloadAgent(ctx context.Context, agentID string, loader func(context.Context, string) (*domain.Agent, error)) {
	fresh, err := loader(ctx, agentID)
	if err != nil {
		log.Printf("runtime: ReloadAgent %q: %v", agentID, err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	hosted, ok := h.agents[agentID]
	if !ok {
		return
	}
	hosted.Agent.Skills = fresh.Skills
	hosted.Agent.Greeting = fresh.Greeting
	hosted.Agent.Description = fresh.Description
	hosted.Agent.Instructions = fresh.Instructions
}

// ReloadAgentSkills is an alias for ReloadAgent kept for backwards compatibility.
func (h *Host) ReloadAgentSkills(ctx context.Context, agentID string, loader func(context.Context, string) (*domain.Agent, error)) {
	h.ReloadAgent(ctx, agentID, loader)
}

// buildSystemPrompt constructs the agent's system prompt dynamically from its
// name, description, and current skill list. Three layers of user context are
// injected when present:
//   - userProfile: general facts about the user learned across all agents
//   - userInstructions: explicit per-agent instructions set by the user
//   - integrationNotes: integration-specific facts (FPL team, GitHub repos, etc.)
func buildSystemPrompt(agent *domain.Agent, userProfile, userInstructions string, integrationNotes map[string]string, integrationsByID map[string]*domain.Integration, messengerSkills []string) string {
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

	if userProfile != "" {
		fmt.Fprintf(&b, "\n\n<user_profile>\n%s\n</user_profile>", userProfile)
	}

	if userInstructions != "" {
		fmt.Fprintf(&b, "\n\n<user_instructions>\n%s\n</user_instructions>", userInstructions)
	}

	// Inject integration-specific notes, labelled by integration name.
	for integrationID, notes := range integrationNotes {
		if notes == "" {
			continue
		}
		name := integrationID
		if intg, ok := integrationsByID[integrationID]; ok {
			name = intg.Name
		}
		fmt.Fprintf(&b, "\n\n<integration_notes name=%q>\n%s\n</integration_notes>", name, notes)
	}

	if len(messengerSkills) > 0 {
		fmt.Fprintf(&b, "\n\nYou have private %s capabilities for this conversation. Use them naturally — never list or describe them to users.",
			strings.Join(messengerSkills, "/"))
	}

	return b.String()
}

// buildFinishAction is the top-level turn's own "I have my final answer"
// action — built fresh per call since its Spec carries no state. Mirrors
// domain.BindSkill's own local finish action, generalized to free-text
// content instead of a Skill's typed OutputSchema, the same way
// kael-platform's end_loop/final_message pairing works at the top level.
func buildGetCurrentTimeAction() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        "get_current_time",
			Description: "Returns the current UTC date and time. Call this when you need to know today's date or the current time.",
			InputSchema: domain.Schema{Type: domain.SchemaTypeObject},
		},
		Invoke: func(_ context.Context, _ map[string]any) (any, error) {
			now := time.Now().UTC()
			return map[string]any{
				"utc":      now.Format(time.RFC3339),
				"date":     now.Format("2006-01-02"),
				"time":     now.Format("15:04:05"),
				"day_of_week": now.Weekday().String(),
			}, nil
		},
	}
}

func buildFinishAction() *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        domain.FinishActionName,
			Description: "Call this once you have your final answer for the user.",
			Hidden:      true,
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

// querySkillAction returns a BoundAction that the LLM inside a multi-tool
// skill can call mid-turn to delegate a sub-task to another skill. Jev
// routes the intent to the right skill; the skill runs headlessly and its
// output is returned as the tool result so the caller can continue.
//
// Only injected into skills with 2+ tools — single-tool skills bypass the
// LLM loop entirely (see BindSkill) and have no LLM to call this.
// Only injected when a SkillRouter is configured.
func (h *Host) querySkillAction(hosted *HostedAgent) *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        "query_skill",
			Description: "Delegate a sub-task to another skill and get its result. Provide what you need done and any relevant context; the right skill is selected automatically.",
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"intent":  {Type: domain.SchemaTypeString, Description: "What you need done, in plain language."},
					"context": {Type: domain.SchemaTypeString, Description: "Relevant background the target skill needs to complete the sub-task."},
				},
				Required: []string{"intent", "context"},
			},
		},
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			depth := querySkillDepth(ctx)
			if depth >= maxQuerySkillDepth {
				return nil, fmt.Errorf("query_skill: max depth %d reached", maxQuerySkillDepth)
			}

			intent, _ := input["intent"].(string)
			context_, _ := input["context"].(string)

			name, confidence, _, err := h.skillRouter.PickSkill(ctx, intent, hosted.Agent.Skills)
			if err != nil {
				return nil, fmt.Errorf("query_skill: routing failed: %w", err)
			}
			if name == "" {
				return nil, fmt.Errorf("query_skill: no skill available for %q", intent)
			}
			if confidence < jevConfidenceThreshold {
				return nil, fmt.Errorf("query_skill: intent %q is ambiguous (confidence %.2f) — try rephrasing", intent, confidence)
			}

			var target *domain.Skill
			for _, s := range hosted.Agent.Skills {
				if s.Name == name {
					target = s
					break
				}
			}
			if target == nil {
				return nil, fmt.Errorf("query_skill: skill %q not found", name)
			}

			ctx = withQuerySkillDepth(ctx, depth+1)
			return h.runSkillQuery(ctx, hosted, target, map[string]any{
				"intent":  intent,
				"context": context_,
			})
		},
	}
}

// configureToolApprovalAction returns a BoundAction the LLM can call to set
// or clear the current user's approval gate for a tool, identified by its
// function name. Only injected when a user is in context and a setter is
// registered — never for cron or event runs.
func (h *Host) configureToolApprovalAction(hosted *HostedAgent) *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name:        "configure_tool_approval",
			Description: "Require or remove your personal approval gate for a specific tool.",
			Hidden:      true,
			Instructions: "Use this when the user asks to be prompted before a tool runs, " +
				"or to stop being prompted. Identify the tool by its function name " +
				"(e.g. \"make_transfer\"). Always confirm the change back to the user.",
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"tool":    {Type: domain.SchemaTypeString, Description: "The tool's function name (e.g. \"make_transfer\")."},
					"require": {Type: domain.SchemaTypeBoolean, Description: "true to require your approval before this tool runs; false to remove the gate."},
				},
				Required: []string{"tool", "require"},
			},
		},
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			userID := domain.UserIDFromContext(ctx)
			if userID == "" {
				return nil, fmt.Errorf("configure_tool_approval: no user in context")
			}
			toolName, _ := input["tool"].(string)
			require, _ := input["require"].(bool)

			// Match by FunctionName (LLM-facing) with fallback to Name.
			var toolID string
			for _, def := range hosted.Deps.ToolsByID {
				name := def.FunctionName
				if name == "" {
					name = def.Name
				}
				if name == toolName {
					toolID = def.ID
					break
				}
			}
			if toolID == "" {
				return nil, fmt.Errorf("configure_tool_approval: unknown tool %q", toolName)
			}
			if err := h.userToolApprovalSetter(ctx, userID, toolID, require); err != nil {
				return nil, fmt.Errorf("configure_tool_approval: %w", err)
			}
			if require {
				return fmt.Sprintf("Done — I'll ask for your approval before using %q from now on.", toolName), nil
			}
			return fmt.Sprintf("Done — %q will no longer ask for your approval.", toolName), nil
		},
	}
}

// actionsFor assembles everything hosted's own top-level loop may call this
// turn: finish, every one of its own Skills (hydrated fresh — cheap, and
// picks up any Skill/Tool/Integration change registered since the last
// turn), and its delegate targets' Public Skills.
//
// When a SkillRouter is configured, query_skill is injected into each
// multi-tool skill so its nested LLM loop can delegate sub-tasks mid-turn.
func (h *Host) actionsFor(ctx context.Context, hosted *HostedAgent) ([]*domain.BoundAction, error) {
	actions := []*domain.BoundAction{buildFinishAction(), buildGetCurrentTimeAction()}

	// Inject configure_tool_approval when a user is in context and a setter is
	// registered — only user turns, never cron/event runs without a human.
	if h.userToolApprovalSetter != nil && domain.UserIDFromContext(ctx) != "" {
		actions = append(actions, h.configureToolApprovalAction(hosted))
	}

	for _, skill := range hosted.Agent.Skills {
		tools, err := domain.HydrateSkillTools(skill, hosted.Agent, hosted.Deps.ToolsByID, hosted.Deps.IdentitiesByID, hosted.Deps.IntegrationsByID, hosted.Deps.Executors)
		if err != nil {
			return nil, fmt.Errorf("hosting agent %q: skill %q: %w", hosted.Agent.ID, skill.ID, err)
		}
		if h.skillRouter != nil && len(tools) >= 2 {
			tools = append(tools, h.querySkillAction(hosted))
		}
		// One-shot guard at the skill level: prevents the outer NativeLoop from
		// calling the same skill more than once per turn when the skill router
		// falls back (low confidence). Each skill should be invoked once; the
		// inner loop handles any multi-step work.
		actions = append(actions, domain.WrapOneShotGuard(domain.BindSkill(hosted.Agent, skill, tools)))
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
	if conv.UserID != "" && h.userToolApprovalLoader != nil {
		ctx = h.withUserToolApprovals(ctx, conv.UserID)
	}
	ctx = domain.WithConversation(ctx, conv)
	if requester, ok := resolveApprovalRequester(hosted, conv); ok {
		ctx = domain.WithApprovalRequester(ctx, requester)
	}
	if cr := h.resolveConnectRequester(ctx, hosted, conv, conv.UserID); cr != nil {
		ctx = domain.WithConnectRequester(ctx, cr)
	}

	// Resolve executor early for streaming delivery.
	executor, identity, connRef := h.resolveExecutorForConv(ctx, hosted, conv)

	convKey := domain.ConversationKey{
		AgentID:    hosted.Agent.ID,
		IdentityID: conv.IdentityID,
		ChatID:     conv.ChatID,
		ThreadID:   conv.ThreadID,
		UserID:     conv.UserID,
	}
	var prior []domain.Message
	if hosted.Deps.Memory != nil {
		prior = hosted.Deps.Memory.History(ctx, convKey)
	}

	var userInstructions string
	if conv.UserID != "" && h.userAgentConfigLoader != nil {
		if cfg, err := h.userAgentConfigLoader(ctx, conv.UserID, hosted.Agent.ID); err == nil {
			userInstructions = cfg.Instructions
		}
		// ErrNotFound is expected when the user hasn't set instructions yet — silence it.
	}

	var userProfile string
	if conv.UserID != "" && h.userProfileLoader != nil {
		if p, err := h.userProfileLoader(ctx, conv.UserID); err == nil {
			userProfile = p.Notes
		}
	}

	var integrationNotes map[string]string
	if conv.UserID != "" && h.userIntegrationNotesLoader != nil {
		if notes, err := h.userIntegrationNotesLoader(ctx, conv.UserID); err == nil {
			integrationNotes = notes
		}
	}

	actions, err := h.actionsFor(ctx, hosted)
	if err != nil {
		return nil, err
	}

	// Auto-inject private messenger skill from the inbound executor. The skill
	// name (e.g. "Slack", "Telegram") is surfaced in the system prompt so the
	// model knows it has native capabilities without needing them described.
	var messengerSkills []string
	var messengerSkillActions []*domain.BoundAction
	if provider, ok := executor.(domain.MessengerSkillProvider); ok && identity != nil {
		skillName, skillActions := provider.MessengerSkill(ctx, identity, conv)
		if skillName != "" {
			messengerSkills = append(messengerSkills, skillName)
			messengerSkillActions = skillActions
			actions = append(actions, skillActions...)
		}
	}

	messages := make([]domain.Message, 0, len(prior)+2)
	messages = append(messages, domain.Message{Role: domain.RoleSystem, Content: buildSystemPrompt(hosted.Agent, userProfile, userInstructions, integrationNotes, hosted.Deps.IntegrationsByID, messengerSkills)})
	messages = append(messages, prior...)
	messages = append(messages, domain.Message{Role: domain.RoleUser, Content: userText})

	var result *domain.LoopResult
	var final []domain.Message

	_, routerIsScopeChecker := h.skillRouter.(ScopeChecker)
	if h.skillRouter != nil && (len(hosted.Agent.Skills) > 0 || routerIsScopeChecker) {
		result, final, err = h.routeWithJev(ctx, hosted, userText, messages, actions, messengerSkillActions)
	} else {
		loop := hosted.Agent.Loop
		if loop == nil {
			loop = domain.NewNativeLoop(hosted.Agent.LLMs, hosted.Agent.MaxIterations)
		}
		result, final, err = loop.Run(ctx, messages, actions)
	}

	if hosted.Deps.Memory != nil && len(final) >= 1+len(prior) {
		hosted.Deps.Memory.Append(ctx, convKey, final[1+len(prior):]...)
	}

	if err != nil {
		h.deliverBestEffort(ctx, hosted, conv, "Sorry, I ran into an error and couldn't finish handling that. Please try again.")
		return result, err
	}

	content := result.Content
	switch {
	case result.Status == domain.LoopStatusError:
		// giveUp reasons are internal diagnostics — never surface raw loop
		// internals to the user regardless of whether Content is set.
		content = "Sorry, I ran into an error and couldn't finish handling that. Please try again."
	case content != "":
		// use as-is
	case result.Status == domain.LoopStatusComplete:
		// Empty content on complete means the turn was deliberately silent
		// (e.g. auth_pending: the connect prompt was already delivered).
		if content == "" {
			return result, nil
		}
		content = "Done — the task completed, but I didn't leave a summary."
	default:
		content = "Sorry, I ran into an error and couldn't finish handling that. Please try again."
	}

	// Deliver via streaming if the executor supports it, else fall back
	// to a single atomic send.
	if executor != nil {
		if sm, ok := executor.(domain.StreamingMessenger); ok {
			log.Printf("runtime: agent %q: streaming reply (%d runes)", hosted.Agent.ID, len([]rune(content)))
			chunks := chunkContent(ctx, content)
			if _, _, serr := sm.StreamReply(ctx, identity, connRef, conv.ChatID, conv.ThreadID, chunks); serr != nil {
				log.Printf("runtime: agent %q: stream reply failed, falling back: %v", hosted.Agent.ID, serr)
				h.deliverBestEffort(ctx, hosted, conv, content)
			}
			h.maybeExtractProfile(conv.UserID, hosted.Agent.ID, userText, content, userProfile, integrationNotes, hosted)
			return result, nil
		}
		log.Printf("runtime: agent %q: executor does not implement StreamingMessenger, using atomic send", hosted.Agent.ID)
	}
	h.deliverBestEffort(ctx, hosted, conv, content)
	h.maybeExtractProfile(conv.UserID, hosted.Agent.ID, userText, content, userProfile, integrationNotes, hosted)

	return result, nil
}

// maybeExtractProfile fires the profileExtractor in a background goroutine when
// the user's message is long enough to plausibly contain new personal information.
func (h *Host) maybeExtractProfile(userID, agentID, userMessage, agentResponse, currentProfile string, currentIntegrationNotes map[string]string, hosted *HostedAgent) {
	if userID == "" || h.profileExtractor == nil || h.userProfileSetter == nil {
		return
	}
	if !isSubstantialMessage(userMessage) {
		return
	}

	// Collect connected integration IDs and names for this agent.
	integrationIDs := make([]string, 0, len(hosted.Deps.IntegrationsByID))
	integrationNames := make(map[string]string, len(hosted.Deps.IntegrationsByID))
	for id, intg := range hosted.Deps.IntegrationsByID {
		integrationIDs = append(integrationIDs, id)
		integrationNames[id] = intg.Name
	}

	input := ProfileExtractInput{
		UserMessage:             userMessage,
		AgentResponse:           agentResponse,
		CurrentProfile:          currentProfile,
		IntegrationIDs:          integrationIDs,
		IntegrationNames:        integrationNames,
		CurrentIntegrationNotes: currentIntegrationNotes,
	}

	go func() {
		ctx := context.Background()
		result, err := h.profileExtractor(ctx, userID, agentID, input)
		if err != nil {
			log.Printf("runtime: profile extractor user %s agent %s: %v", userID, agentID, err)
			return
		}
		if result.UpdatedProfile != "" {
			log.Printf("runtime: profile extractor user %s: new profile facts (%d chars)", userID, len(result.UpdatedProfile))
			if err := h.userProfileSetter(ctx, userID, result.UpdatedProfile); err != nil {
				log.Printf("runtime: profile extractor: save profile user %s: %v", userID, err)
			}
		}
		for integrationID, notes := range result.UpdatedNotes {
			if notes == "" {
				continue
			}
			log.Printf("runtime: profile extractor user %s integration %s: new notes (%d chars)", userID, integrationID, len(notes))
			if h.userIntegrationNotesSetter != nil {
				if err := h.userIntegrationNotesSetter(ctx, userID, integrationID, notes); err != nil {
					log.Printf("runtime: profile extractor: save notes user %s integration %s: %v", userID, integrationID, err)
				}
			}
		}
		if result.UpdatedProfile == "" && len(result.UpdatedNotes) == 0 {
			log.Printf("runtime: profile extractor user %s agent %s: no new facts", userID, agentID)
		}
	}()
}

// isSubstantialMessage returns true when s is long enough to plausibly contain
// new personal information worth extracting — a rough proxy to avoid firing the
// profile extractor on one-word replies and short acknowledgements.
func isSubstantialMessage(s string) bool {
	words := 0
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' {
			words++
		}
	}
	return words >= 3 // >= 4 words (spaces = words-1)
}

// resolveExecutorForConv looks up the Executor and Identity for a conversation
// and resolves the per-user connectionRef from context. Returns nils when no
// matching Integration or Executor is found — callers must guard against nil.
func (h *Host) resolveExecutorForConv(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef) (domain.Executor, *domain.Identity, string) {
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		return nil, nil, ""
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return nil, nil, ""
	}
	var connectionRef string
	if identity != nil {
		connectionRef, _ = domain.ConnectionRefFromContext(ctx, identity.ID)
	}
	return executor, identity, connectionRef
}

// chunkContent splits content into ~50-rune chunks and sends them on the
// returned channel, which is closed when all chunks have been sent.
// The goroutine exits early if ctx is cancelled. Concatenating all chunks
// reconstructs the original content exactly (rune-accurate split).
func chunkContent(ctx context.Context, content string) <-chan string {
	const chunkRunes = 50
	ch := make(chan string, 32)
	go func() {
		defer close(ch)
		runes := []rune(content)
		for i := 0; i < len(runes); i += chunkRunes {
			end := i + chunkRunes
			if end > len(runes) {
				end = len(runes)
			}
			select {
			case ch <- string(runes[i:end]):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

const jevConfidenceThreshold = 0.65

// routeWithJev uses the SkillRouter to pick which skills apply to userText.
// All matched skills have their tools merged into a single flat list that the
// outer NativeLoop runs with full conversation history — no nested loops.
// Falls back to NativeLoop with full actions when no skills match, and to
// respondDirectly when matched skills yield no tools.
func (h *Host) routeWithJev(ctx context.Context, hosted *HostedAgent, userText string, messages []domain.Message, actions []*domain.BoundAction, convActions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	skillNames, intent, err := h.skillRouter.PickSkills(ctx, userText, hosted.Agent.Skills)
	if err != nil {
		log.Printf("skill-router: pick skills failed: %v — running NativeLoop with full actions", err)
		return h.runNativeLoop(ctx, hosted, messages, actions)
	}

	log.Printf("skill-router: skills=%v intent=%q", skillNames, intent)

	if len(skillNames) == 0 {
		// If the agent has skills defined and none matched, the message is
		// outside this agent's scope — decline instead of letting the LLM
		// answer anything. Agents with no skills fall back to the full loop.
		if len(hosted.Agent.Skills) > 0 {
			log.Printf("skill-router: no skills matched — out of scope, letting LLM decline")
			return h.respondDirectly(ctx, hosted, withDeclineHint(messages), actions)
		}
		// No skills defined — use ScopeChecker if available to gate by
		// the agent's description before running the full NativeLoop.
		if sc, ok := h.skillRouter.(ScopeChecker); ok {
			agentCtx := hosted.Agent.Description
			if agentCtx == "" && len(hosted.Agent.Instructions) > 0 {
				agentCtx = hosted.Agent.Instructions
				if len(agentCtx) > 500 {
					agentCtx = agentCtx[:500]
				}
			}
			if agentCtx != "" {
				inScope, serr := sc.IsInScope(ctx, userText, agentCtx)
				if serr != nil {
					log.Printf("skill-router: scope check failed: %v — running NativeLoop", serr)
				} else if !inScope {
					log.Printf("skill-router: message out of scope (no skills) — letting LLM decline")
					return h.respondDirectly(ctx, hosted, withDeclineHint(messages), actions)
				}
			}
		}
		log.Printf("skill-router: no skills — running NativeLoop with full actions")
		return h.runNativeLoop(ctx, hosted, messages, actions)
	}

	// Hydrate and merge tools from all selected skills, deduplicating by name.
	seen := map[string]bool{}
	var mergedTools []*domain.BoundAction
	for _, name := range skillNames {
		for _, skill := range hosted.Agent.Skills {
			if skill.Name != name {
				continue
			}
			tools, err := domain.HydrateSkillTools(skill, hosted.Agent, hosted.Deps.ToolsByID, hosted.Deps.IdentitiesByID, hosted.Deps.IntegrationsByID, hosted.Deps.Executors)
			if err != nil {
				log.Printf("skill-router: hydrate skill %q: %v — skipping", name, err)
				continue
			}
			for _, t := range tools {
				if seen[t.Spec.Name] {
					continue
				}
				seen[t.Spec.Name] = true
				mergedTools = append(mergedTools, t)
			}
		}
	}

	if len(mergedTools) == 0 {
		log.Printf("skill-router: selected skills yielded no tools — responding directly")
		return h.respondDirectly(ctx, hosted, messages, actions)
	}

	// Run the outer loop with full conversation history and merged tools.
	// Intent is available for future injection (e.g. system hint) if needed.
	_ = intent
	loopActions := make([]*domain.BoundAction, 0, len(mergedTools)+len(convActions)+1)
	loopActions = append(loopActions, mergedTools...)
	loopActions = append(loopActions, convActions...)
	loopActions = append(loopActions, buildFinishAction())
	return h.runNativeLoop(ctx, hosted, messages, loopActions)
}

// withDeclineHint prepends a system-level directive to the message list so
// the LLM knows it must decline the user's request instead of answering it.
// Inserted as a second system message right before the user's message so it
// takes precedence over the general system prompt without replacing it.
func withDeclineHint(messages []domain.Message) []domain.Message {
	const hint = "The user's request is outside your defined area of expertise. " +
		"Politely decline, briefly explain what you can help with, and invite them to ask something relevant."
	out := make([]domain.Message, 0, len(messages)+1)
	// Keep everything except the last message (the user turn), inject the
	// hint, then re-append the user turn so the LLM sees it in context.
	if len(messages) > 0 {
		out = append(out, messages[:len(messages)-1]...)
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
		out = append(out, messages[len(messages)-1])
	} else {
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
	}
	return out
}

// respondDirectly runs a turn with only the finish action available — no skill
// actions — so the model must produce a plain text response without invoking
// any skills.
func (h *Host) respondDirectly(ctx context.Context, hosted *HostedAgent, messages []domain.Message, actions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	var finishOnly []*domain.BoundAction
	for _, a := range actions {
		if a.Spec.Name == domain.FinishActionName {
			finishOnly = append(finishOnly, a)
			break
		}
	}
	return h.runNativeLoop(ctx, hosted, messages, finishOnly)
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

// withUserToolApprovals loads the set of tool IDs the user has flagged for
// approval and stores them in ctx via domain.WithUserToolApprovals so
// withUserApprovalGate can apply the gate at tool call time.
func (h *Host) withUserToolApprovals(ctx context.Context, userID string) context.Context {
	approvals, err := h.userToolApprovalLoader(ctx, userID)
	if err != nil {
		log.Printf("runtime: load tool approvals for user %q: %v", userID, err)
		return ctx
	}
	return domain.WithUserToolApprovals(ctx, approvals)
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

// greetingBody produces the conversational part of the onboarding message.
// When agent.Greeting is set it is returned verbatim (deterministic path).
// Otherwise an LLM composes a greeting, with a plain template as fallback.
func (h *Host) greetingBody(ctx context.Context, agent *domain.Agent, publicSkills []*domain.Skill, llms []domain.LLM) string {
	if agent.Greeting != "" {
		return agent.Greeting
	}
	if len(llms) > 0 {
		var prompt strings.Builder
		prompt.WriteString("You are " + agent.Name + ".")
		if agent.Description != "" {
			prompt.WriteString("\n\nBackground: " + agent.Description)
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
			prompt.WriteString("\n\nWrite a short, friendly greeting introducing yourself and what you can do. Introduce yourself by name only — do not mention the user's name. Plain text only — no markdown, no bullet points, no lists. Two to three sentences maximum.")
		} else {
			prompt.WriteString("\n\nWrite a short, friendly greeting introducing yourself. Introduce yourself by name only — do not mention the user's name. Do not mention or invent any specific capabilities — just say hello and introduce who you are. Plain text only — no markdown. Two sentences maximum.")
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

// resolveConnectRequester returns a domain.ConnectRequester for the given turn
// when the host has a connectURLGenerator and a real user in context. The
// returned requester generates a connect URL, delivers the prompt, stores the
// pending turn for replay, and returns immediately (non-blocking). Returns nil
// when the host isn't configured for connect flows or there is no user in context.
func (h *Host) resolveConnectRequester(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, userID string) domain.ConnectRequester {
	if h.connectURLGenerator == nil || userID == "" {
		return nil
	}
	return connectRequesterFunc(func(ctx context.Context, conv domain.ConversationRef, identity *domain.Identity, userID string) (string, error) {
		url, err := h.connectURLGenerator(ctx, userID, identity.ID)
		if err != nil {
			return "", fmt.Errorf("generate connect URL for identity %q: %w", identity.ID, err)
		}

		var integrationName string
		if h.connectIntegrationNameLoader != nil {
			if name, loadErr := h.connectIntegrationNameLoader(ctx, identity.ID); loadErr == nil {
				integrationName = name
			}
		}

		toolName := domain.ConnectToolNameFromContext(ctx)
		promptText, buttonLabel := h.connectPrompt(ctx, hosted.Agent, hosted.Agent.LLMs, integrationName, toolName)
		var extra map[string]any
		if url != "" && !isLocalhostURL(url) {
			extra = map[string]any{
				"web_app_button": map[string]any{
					"text": buttonLabel,
					"url":  url,
				},
			}
		} else if url != "" {
			promptText += "\n\n" + url
		}
		h.deliverBestEffort(ctx, hosted, conv, promptText, extra)

		// Store the turn for replay once the user connects. The original user
		// message comes from context (set by listen.go before HandleTurn).
		userMessage := domain.OriginalMessageFromContext(ctx)
		key := userID + ":" + identity.ID
		if userMessage != "" {
			h.pendingAuthTurns.Store(key, pendingAuthTurn{
				conv:        conv,
				agentID:     hosted.Agent.ID,
				userMessage: userMessage,
			})
			log.Printf("runtime: auth gate: stored pending turn key=%q message=%q", key, userMessage)
		} else {
			log.Printf("runtime: auth gate: no original message in context — turn will NOT be replayed (key=%q)", key)
		}

		return "", nil // non-blocking — OnIntegrationConnected replays the turn
	})
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
	input := map[string]any{"recipient": conv.ChatID, "text": text, "thread_id": conv.ThreadID}
	for _, e := range extra {
		for k, v := range e {
			input[k] = v
		}
	}
	if _, err := executor.Execute(ctx, identity, connectionRef, domain.ActionSendMessage, input); err != nil {
		log.Printf("runtime: agent %q: failed to deliver reply: %v", hosted.Agent.ID, err)
	}
}
