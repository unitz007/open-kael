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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
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
// actually run: the Catalog it reads tools, identities and integrations from,
// the executors, and an optional Memory. Kept separate from domain.Agent because
// these are runtime wiring, not stored agent data.
type AgentDeps struct {
	// Catalog is the one live list every agent shares. Set it to share a catalog
	// across agents; when nil, one is built from the three maps below the first time
	// it is needed.
	Catalog *domain.Catalog

	// ToolsByID, IdentitiesByID and IntegrationsByID seed a Catalog when Catalog is
	// nil (tests, small programs). Nothing reads them afterwards.
	ToolsByID        map[string]*domain.ToolDefinition
	IdentitiesByID   map[string]*domain.Identity
	IntegrationsByID map[string]*domain.Integration

	Executors *domain.ExecutorRegistry
	// Memory is optional — nil means every turn starts cold, same as
	// domain.BindSkill's nested loop does today.
	Memory domain.Memory
}

// HostedAgent is one Agent as the runtime Host actually runs it.
type HostedAgent struct {
	Agent *domain.Agent
	Deps  AgentDeps

	catalogOnce sync.Once
}

// catalog is the agent's view of tools, identities and integrations. Everything
// that looks one up goes through it, so a connection added while the server runs is
// seen everywhere at once.
func (h *HostedAgent) catalog() *domain.Catalog {
	h.catalogOnce.Do(func() {
		if h.Deps.Catalog != nil {
			return
		}
		c := domain.NewCatalog()
		for _, i := range h.Deps.IntegrationsByID {
			c.PutIntegration(i)
		}
		for _, i := range h.Deps.IdentitiesByID {
			c.PutIdentity(i)
		}
		for _, t := range h.Deps.ToolsByID {
			c.PutTool(t)
		}
		h.Deps.Catalog = c
	})
	return h.Deps.Catalog
}

// Catalog is the catalog this agent reads (shared with every other agent when it
// came from Boot). Code that adds or removes connections while the server runs
// registers them here.
func (h *HostedAgent) Catalog() *domain.Catalog { return h.catalog() }

// owner is the creator this agent belongs to ("" for a platform agent).
func (h *HostedAgent) owner() string { return h.Agent.CreatedBy }

// View returns what this agent may use, in one consistent pass: every integration
// its creator may use (the platform's and their own, never another creator's) with
// those integrations' identities and tools.
func (h *HostedAgent) View() (tools map[string]*domain.ToolDefinition, identities map[string]*domain.Identity, integrations map[string]*domain.Integration) {
	c := h.catalog()
	integrations, identities, tools = c.Integrations(), c.Identities(), c.Tools()
	for id, i := range integrations {
		if !domain.VisibleTo(i, h.owner()) {
			delete(integrations, id)
		}
	}
	for id, i := range identities {
		if _, ok := integrations[i.IntegrationID]; !ok {
			delete(identities, id)
		}
	}
	for id, t := range tools {
		if _, ok := integrations[t.IntegrationID]; !ok {
			delete(tools, id)
		}
	}
	return tools, identities, integrations
}

func (h *HostedAgent) integrations() map[string]*domain.Integration { _, _, i := h.View(); return i }
func (h *HostedAgent) identities() map[string]*domain.Identity      { _, i, _ := h.View(); return i }
func (h *HostedAgent) tools() map[string]*domain.ToolDefinition     { t, _, _ := h.View(); return t }

// Host runs a set of registered agents' turns, cron schedules, and event
// dispatch. The zero value is not ready to use — call NewHost.
type Host struct {
	mu     sync.RWMutex
	agents map[string]*HostedAgent

	// Cron scheduler state, kept in step with the agents (see StartCron).
	// cronMu is only ever taken after h.mu, never before.
	cronMu      sync.Mutex
	sched       *cron.Cron
	cronCtx     context.Context
	cronEntries map[string][]cron.EntryID // agentID -> its scheduled entries

	// listenerCancels stores a cancel func for each running listener goroutine,
	// keyed by "agentID:identityID". Allows individual listeners to be stopped
	// when an identity is removed from an agent (see StopIdentityListeners).
	listenerCancels sync.Map

	// agentInboxes stores the message inbox per agentID so dynamic listeners
	// added via StartListenerForAgent can feed into the same channel as the
	// agent's existing processing goroutine.
	agentInboxes sync.Map

	// convQueues holds, per conversation, the messages waiting for the one being handled
	// (see dispatchInbound). A conversation with nothing in flight has no entry.
	convMu     sync.Mutex
	convQueues map[string]*convQueue

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

	// connectPromptHook, when set, is told about every connect prompt sent by
	// the auth gate (see SetConnectPromptHook).
	connectPromptHook func(ctx context.Context, p ConnectPrompt)

	// userChannelsByIdentityLoader, when set, returns all MessengerChannel
	// channel refs for the given (userID, identityID) pair. Used by
	// OnIntegrationConnected to deliver post-connection messages.
	userChannelsByIdentityLoader func(ctx context.Context, userID, identityID string) (channelRefs []string, err error)

	// toolRequirer, when set, decides which action a request to DO something needs,
	// so the agent can be held to it. See ToolRequirer.
	toolRequirer ToolRequirer

	// pendingAuthTurns tracks turns that were interrupted by the auth gate.
	// Key is "userID:identityID"; value is pendingAuthTurn.
	// When OnIntegrationConnected fires, the stored turn is replayed in a
	// fresh goroutine so the agent can continue where it left off.
	pendingAuthTurns sync.Map

	// pendingTurnStore, when set, keeps held requests across restarts.
	pendingTurnStore PendingTurnStore

	// welcomeMessageStore remembers which message carries the Get started button.
	welcomeMessageStore WelcomeMessageStore

	// carry remembers which abilities each conversation was just using, so a short
	// follow-up that matches nothing on its own stays with them.
	carry *carryOver

	// repeatingTasksNote describes a person's repeating tasks for the agent's instructions.
	repeatingTasksNote func(ctx context.Context, userID, agentID string) string

	// greetingJudge tells a bare hello from a real message (see GreetingJudge).
	greetingJudge GreetingJudge

	// userDetailsLoader looks up what a person has told us about themselves.
	userDetailsLoader func(ctx context.Context, userID string) (domain.UserDetails, error)

	// connectPromptSent remembers when a person was last sent a connect prompt for an
	// identity ("userID:identityID"), so several tool calls made in the same moment
	// send ONE prompt instead of one each. See shouldSendConnectPrompt.
	connectPromptMu   sync.Mutex
	connectPromptSent map[string]time.Time

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

	// readiness, when set, says why an agent cannot answer people yet ("" when it can).
	readiness func(ctx context.Context, agent *domain.Agent) string

	// frontendURL is the base URL of the creator/user-facing web app. When
	// set, unlinked users receive an onboarding message that includes this URL
	// so they know where to sign up and get their link code.
	frontendURL string

	// miniAppBaseURL is the base URL at which the Telegram Mini App is served
	// (e.g. "https://myapp.fly.dev"). When set, the /settings command sends a
	// web_app button pointing to <miniAppBaseURL>/miniapp/settings?agent=<agentID>
	// instead of the inline-keyboard SettingsFlow.
	miniAppBaseURL string

	// skillRouter, when set, routes inbound messages to the correct skill before
	// the inner skill loop — replacing the outer NativeLoop's LLM-based skill
	// selection with a fast, type-safe classifier.
	skillRouter SkillRouter

	// toolFilter, when set, trims the merged tool list before it is passed to
	// the LLM — reducing token usage and narrowing the LLM's tool choices to
	// what is actually relevant for the current turn.
	toolFilter ToolFilter

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

	// emailLinkChecker reports whether the user already has a verified email.
	emailLinkChecker func(ctx context.Context, userID string) (bool, error)

	// emailLinkStateGetter returns the current email_link_state for a channel.
	emailLinkStateGetter func(ctx context.Context, identityID, channelRef string) (string, error)

	// emailLinkStateSetter persists email_link_state changes so they survive restarts.
	emailLinkStateSetter func(ctx context.Context, identityID, channelRef, state string) error

	// emailLinkInitiator validates the email, creates a verification record, and
	// sends the verification email. Called when the user provides their email address.
	emailLinkInitiator func(ctx context.Context, userID, identityID, channelRef, email string) error

	// pendingEmailLinks (channel → the time it was asked) tracks channels that have been prompted for an email and
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

	// Details a person has shared, learned one at a time (see profile_host.go).
	profileDetails profileSetup

	// proposals keeps the choices an agent offers as buttons (see proposal_host.go); proposalDispatch hands a
	// tapped choice to the conversation as if it had been typed.
	proposals        domain.ProposalStore
	choiceModel      domain.LLM // the platform's own model for turning an offer into choices; the agent's own when unset
	proposalDispatch func(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage)

	// intentHandlers maps a domain.Intent to a handler that intercepts turns
	// before the skill loop. The handler receives the matched skill name (empty
	// when no skill was matched) and returns a reply to deliver. When the
	// handler returns an error the turn falls through to the skill loop.
	intentHandlers map[domain.Intent]IntentHandler

	// activity, when set, is told how each agent is doing (see activity.go): counts only.
	activity     domain.ActivityRecorder
	topicLabeler TopicLabeler

	// routeByTools lets a ToolAwareSkillRouter pick abilities by what their tools do.
	routeByTools bool

	// timezoneLink makes the link a person follows to set their time zone (see SetTimezoneLink).
	timezoneLink func(ctx context.Context, userID, agentID, provider string) (string, error)
}

// IntentHandler is a function that handles a turn for a specific intent before
// the skill loop runs. skillName is the Jev-matched skill (empty when no skill
// matched). Return a non-empty reply to deliver it and skip the loop; return an
// error to fall through to the skill loop.
type IntentHandler func(ctx context.Context, userID, agentID, skillName, userText, identityID, channelRef string) (reply string, err error)

// SkillRouter picks the right skill(s) for an inbound message. Any classifier
// — a remote API, a local model, a keyword matcher — can implement this;
// the concrete TypeSafe AI Jev implementation lives in the root jev package.
type SkillRouter interface {
	// PickSkill returns the single best-matching skill name, the classifier's
	// confidence (0–1), and the user's intent. Returns ("", 0, IntentRecommend, nil)
	// when no skill fits. Used by query_skill for single-skill delegation.
	PickSkill(ctx context.Context, userText string, skills []*domain.Skill) (skillName string, confidence float64, intent domain.Intent, err error)

	// PickSkills returns all skills that apply to userText and the user's
	// intent. An empty slice means no skill matched. Used by the main routing
	// path to load and merge tools from multiple skills into one outer loop.
	PickSkills(ctx context.Context, userText string, skills []*domain.Skill) (skillNames []string, intent domain.Intent, err error)
}

// ContextualSkillRouter is an optional extension of SkillRouter. A short follow-up such as
// "yes", "do it" or "pull up the full context" means nothing alone, so when the router also
// implements this, the host passes it what was said just before: the person's previous
// message and the agent's reply to it. Without it (or with nothing earlier) PickSkills is used.
type ContextualSkillRouter interface {
	PickSkillsInContext(ctx context.Context, userText string, earlier domain.Earlier, skills []*domain.Skill) (skillNames []string, intent domain.Intent, err error)
}

// ToolAwareSkillRouter is an optional extension of SkillRouter. A router that implements it is
// shown what each ability's tools do and may pick an ability because one of its tools fits the
// request, not only because the ability's own description does. It is used only when the host's
// routing by tools is switched on (SetRouteByTools); otherwise PickSkills is used as before.
type ToolAwareSkillRouter interface {
	PickSkillsWithTools(ctx context.Context, userText string, earlier domain.Earlier, skills []domain.RoutedSkill) (skillNames []string, intent domain.Intent, err error)
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

// IntentClassifier is an optional extension of SkillRouter. When the configured router also
// implements it, the kind of statement is decided FIRST, on its own, and everything after
// follows from that: a social message is answered warmly, something unrelated to the agent is
// declined, and only the rest go on to find which abilities they need. agentArea is what the
// agent is for (its description); empty means the router can't judge "unrelated".
type IntentClassifier interface {
	ClassifyIntent(ctx context.Context, userText string, earlier domain.Earlier, agentArea string) (domain.Intent, error)
}

// ToolFilter, when set on the Host, trims the merged tool list before it is
// passed to the LLM. It receives the user's message and the full hydrated list,
// and returns the subset that is relevant. The filter must never return an empty
// slice — callers fall back to the full list when it does.
type ToolFilter interface {
	FilterTools(ctx context.Context, userText string, tools []*domain.BoundAction) ([]*domain.BoundAction, error)
}

func NewHost() *Host {
	h := &Host{
		agents:         make(map[string]*HostedAgent),
		bus:            newEventBus(),
		intentHandlers: make(map[domain.Intent]IntentHandler),
		carry:          newCarryOver(),
		convQueues:     make(map[string]*convQueue),
	}
	h.proposalDispatch = h.dispatchInbound
	return h
}

// SetSkillRouter registers a skill router. When set, HandleTurn uses it to
// pick which skill should handle the user's message instead of running the
// outer NativeLoop. Falls back to NativeLoop when the router returns low
// confidence or no matching skill.
func (h *Host) SetSkillRouter(r SkillRouter) {
	h.skillRouter = r
}

// SetToolFilter registers a tool filter. When set, routeWithJev passes the
// merged tool list through the filter before handing it to the LLM — trimming
// irrelevant tools to reduce token usage. Falls back to the full list on error
// or when the filter returns empty.
func (h *Host) SetToolFilter(f ToolFilter) {
	h.toolFilter = f
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

// SetReadinessCheck registers the check that decides whether an agent may answer people. An agent that is not ready
// does not run: whoever writes to it is told, in plain words, that it is not ready yet. The reason the check returns
// is for the creator and is never sent to them.
func (h *Host) SetReadinessCheck(f func(ctx context.Context, agent *domain.Agent) string) { h.readiness = f }

// notReadyReply is what a person is told by an agent that is not ready to answer yet.
const notReadyReply = "I'm not ready to help yet. The person who set me up still has a little more to do, so please try again later."

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

// SetIntentHandler registers a handler for a specific intent. When the skill
// router classifies a turn with the given intent, the handler is called before
// the skill loop. Return a non-empty reply to deliver it and skip the loop;
// return an error to fall through to the skill loop.
func (h *Host) SetIntentHandler(intent domain.Intent, handler IntentHandler) {
	h.intentHandlers[intent] = handler
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

// ConnectPrompt describes a "connect your account" message the auth gate just
// sent: who it went to, where, and which message — so the caller can edit it
// once the person has connected (see SetConnectPromptHook).
type ConnectPrompt struct {
	UserID     string
	IdentityID string // the integration identity the person is being asked to connect
	Conv       domain.ConversationRef
	MessageID  string
	Text       string
}

// SetConnectPromptHook registers a callback invoked after the auth gate sends a
// connect prompt, when the messenger reported the sent message's ID. Typical
// use: remember it, then retire the Connect button after the user connects.
func (h *Host) SetConnectPromptHook(f func(ctx context.Context, p ConnectPrompt)) {
	h.connectPromptHook = f
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
	if pending, ok := h.takeTurn(ctx, userID, identityID); ok {
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
	text := connectedMessage(integrationName)
	conv := domain.ConversationRef{IdentityID: identityID}
	for _, ref := range channelRefs {
		conv.ChatID = ref
		h.deliverBestEffort(ctx, hosted, conv, text)
	}
}

// connectedMessage confirms that a person connected an account. It is fixed text:
// what a person is told at each step of getting started never depends on a model.
func connectedMessage(integrationName string) string {
	if integrationName != "" {
		return "Your " + integrationName + " account is now connected! Feel free to ask me anything."
	}
	return "You're all connected! Feel free to ask me anything."
}

// connectPrompt is what a person is told when an account must be connected before
// the agent can go on. It is fixed text, the same every time. Returns (messageText,
// buttonLabel).
func connectPrompt(integrationName string) (text, buttonLabel string) {
	if integrationName == "" {
		return "To continue, tap the button below to connect the account I need.", "Connect account"
	}
	return "To go ahead, I need you to connect your " + integrationName + " account. Tap the button below.", "Connect " + integrationName + " account"
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
	// A new (or replaced) agent's event and cron skills take effect at once.
	h.bus.resync(hosted)
	h.syncCron(hosted)
}

// Deregister removes a HostedAgent from the host's in-memory map and stops its
// identity listeners. Safe to call while the host is running.
func (h *Host) Deregister(agentID string, identityIDs []string) {
	h.StopIdentityListeners(agentID, identityIDs)
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.agents, agentID)
	h.bus.drop(agentID)
	h.dropCron(agentID)
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
	// Skills changed: bring event subscriptions and cron entries into line.
	h.bus.resync(hosted)
	h.syncCron(hosted)
}

// ReloadAgentSkills is an alias for ReloadAgent kept for backwards compatibility.
func (h *Host) ReloadAgentSkills(ctx context.Context, agentID string, loader func(context.Context, string) (*domain.Agent, error)) {
	h.ReloadAgent(ctx, agentID, loader)
}

// clockNow is the time source for prompts; tests replace it.
var clockNow = time.Now

// proposeGuidance is how an agent moves a conversation forward: it does the harmless looking-up itself,
// and when a change is right it comes with the specific thing, not an open offer.
const proposeGuidance = "Moving things forward: when the next useful step is only to look something up or check something (it changes nothing), " +
	"do it now and tell them what you found; do not offer to. When the next step would change something, do the looking-up first so that " +
	"what you propose is specific (the real time, the real name, the real amount), then propose exactly that. Never end with a vague " +
	"\"want me to help with that?\" or \"let me know if you'd like...\". If there are a few good ways forward, give them as choices when " +
	"your finish action allows it, otherwise ask one short, specific question. When they only asked a question, answer it and stop: " +
	"do not tack on an offer to book, hold, set up or do something they did not ask about, unless you can name the exact thing as a choice. " +
	"Never go ahead with a change without their agreement."

// buildSystemPrompt constructs the agent's system prompt dynamically from its
// name, description, and current skill list. Three layers of user context are
// injected when present:
//   - userProfile: general facts about the user learned across all agents
//   - userInstructions: explicit per-agent instructions set by the user
//   - integrationNotes: integration-specific facts (FPL team, GitHub repos, etc.)
func buildSystemPrompt(agent *domain.Agent, userProfile, userInstructions string, integrationNotes map[string]string, integrationsByID map[string]*domain.Integration, messengerSkills []string, person domain.UserDetails) string {
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

	// Always tell the agent that the platform learns about users, whether or not
	// a profile exists yet. This prevents agents from incorrectly telling users
	// they cannot remember personal information.
	b.WriteString("\n\nThe platform passively learns facts about the user from your conversations and surfaces them to you in <user_profile>, one per line as \"name: value\". When the user shares personal information, acknowledge it naturally — never tell them you cannot remember or store it.")
	b.WriteString("\n\n" + proposeGuidance)
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

	// The agent must always know when it is. Stated here, to the minute, rather than
	// left to a tool the model may never call (routed turns don't offer one). In the
	// person's own time zone when they have told us, otherwise in UTC.
	b.WriteString(domain.PersonNote(person, clockNow()))

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
				"utc":         now.Format(time.RFC3339),
				"date":        now.Format("2006-01-02"),
				"time":        now.Format("15:04:05"),
				"day_of_week": now.Weekday().String(),
			}, nil
		},
	}
}

func buildFinishAction(withOutcome bool) *domain.BoundAction {
	spec := domain.ActionSpec{
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
	}
	if withOutcome {
		// The person asked for something to be DONE. Say how it ended; "done" is only
		// accepted once the action really succeeded.
		spec.Description = "Call this once you have your final answer for the user. You must say how it ended in outcome."
		spec.InputSchema.Properties["outcome"] = domain.Schema{
			Type: domain.SchemaTypeString,
			Enum: []string{domain.OutcomeDone, domain.OutcomeNeedDetails, domain.OutcomeCannot},
			Description: "done: the action you were asked to carry out has really run successfully this turn (its result says so). " +
				"need_details: you need something from the person first, for example a size or a price. " +
				"cannot: you can't do it, for example their account isn't connected or it wasn't approved. " +
				"Never choose done unless the action really succeeded.",
		}
		spec.InputSchema.Required = append(spec.InputSchema.Required, "outcome")
	}
	return &domain.BoundAction{Spec: spec}
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
			for _, def := range hosted.tools() {
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
	actions := []*domain.BoundAction{h.finishFor(ctx, hosted, false), buildGetCurrentTimeAction()}

	// Inject configure_tool_approval when a user is in context and a setter is
	// registered — only user turns, never cron/event runs without a human.
	if h.userToolApprovalSetter != nil && domain.UserIDFromContext(ctx) != "" {
		actions = append(actions, h.configureToolApprovalAction(hosted))
	}

	for _, skill := range hosted.Agent.Skills {
		tools, err := h.hydrate(hosted, skill)
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
		if identity, ok := hosted.catalog().Identity(conv.IdentityID); ok {
			if integration, ok := hosted.catalog().Integration(identity.IntegrationID); ok {
				return identity, integration
			}
		}
	}
	// Fallback: find the first identity whose integration has this service
	for _, identity := range hosted.identities() {
		if intg, ok := hosted.catalog().Integration(identity.IntegrationID); ok {
			if intg.Service == conv.Provider {
				return identity, intg
			}
		}
	}
	return nil, nil
}

// unattendedFailure is what the person is told when a repeating task could not be finished.
const unattendedFailure = "I couldn't finish your repeating task just now. I'll try again next time."

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
	unattended := domain.IsUnattended(ctx)
	if !unattended {
		h.notePersonActive(hosted, conv)
		if conv.UserID != "" {
			h.noteMessage(hosted)
		}
	}
	if !unattended {
		// With nobody there there is no one to approve, answer a question or sign in.
		if requester, ok := resolveApprovalRequester(hosted, conv); ok {
			ctx = domain.WithApprovalRequester(ctx, requester)
		}
		if cr, ok := resolveChoiceRequester(hosted, conv); ok {
			ctx = domain.WithChoiceRequester(ctx, cr)
		}
		if cr := h.resolveConnectRequester(ctx, hosted, conv, conv.UserID); cr != nil {
			ctx = domain.WithConnectRequester(ctx, cr)
		}
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

	userProfile, integrationNotes := h.profileForPrompt(ctx, conv.UserID, hosted)

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
	// Where the messenger can show buttons, the agent may put a short multiple-choice question
	// to the person. It is offered alongside the other conversation actions.
	if cr, ok := domain.ChoiceRequesterFromContext(ctx); ok {
		ask := askOptionsAction(cr)
		actions = append(actions, ask)
		messengerSkillActions = append(messengerSkillActions, ask)
	}

	promptIntegrations := hosted.integrations()
	messages := make([]domain.Message, 0, len(prior)+2)
	var person domain.UserDetails
	if conv.UserID != "" && h.userDetailsLoader != nil {
		if d, err := h.userDetailsLoader(ctx, conv.UserID); err == nil {
			person = d
		}
	}
	// Abilities run as their own loops: they must know who this is and when it is too.
	ctx = domain.WithUserDetails(ctx, person)
	if !unattended && conv.UserID != "" && person.Timezone == "" && h.timezoneLink != nil {
		// Their time zone is not known: anything that depends on their clock asks for it, once.
		ctx = domain.WithTimezoneRequester(ctx, h.timezoneRequester(hosted))
	}
	systemPrompt := buildSystemPrompt(hosted.Agent, userProfile, userInstructions, integrationNotes, promptIntegrations, messengerSkills, person)
	if conv.UserID != "" && h.repeatingTasksNote != nil {
		if note := strings.TrimSpace(h.repeatingTasksNote(ctx, conv.UserID, hosted.Agent.ID)); note != "" {
			systemPrompt += "\n\n" + note
		}
	}
	messages = append(messages, domain.Message{Role: domain.RoleSystem, Content: systemPrompt})
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

	// An unattended turn keeps only its reply: the "message" was the stored task, not something
	// the person wrote, so it doesn't belong in their conversation (their follow-up questions
	// still have the reply to refer to).
	if !unattended && hosted.Deps.Memory != nil && len(final) >= 1+len(prior) {
		hosted.Deps.Memory.Append(ctx, convKey, final[1+len(prior):]...)
	}

	if err != nil {
		if unattended {
			h.deliverBestEffort(ctx, hosted, conv, unattendedFailure)
		} else {
			h.noteEvent(hosted, domain.ActivityFailed, failureDetail(err))
			h.deliverBestEffort(ctx, hosted, conv, failureText(err))
		}
		return result, err
	}

	content := result.Content
	switch {
	case result.Status == domain.LoopStatusError && unattended:
		content = unattendedFailure
	case result.Status == domain.LoopStatusError:
		// giveUp reasons are internal diagnostics — never surface raw loop
		// internals to the user regardless of whether Content is set.
		h.noteEvent(hosted, domain.ActivityFailed, failureProblem)
		content = "Sorry, I ran into an error and couldn't finish handling that. Please try again."
		if domain.ChangeFailed(result.Actions) {
			// The one thing worth saying plainly: it was tried, it failed, nothing changed.
			content = "I tried, but it didn't go through, so nothing was changed. Please try again in a moment."
		}
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

	if unattended && hosted.Deps.Memory != nil {
		hosted.Deps.Memory.Append(ctx, convKey, domain.Message{Role: domain.RoleAssistant, Content: content})
	}

	// Choices the agent finished with: buttons where they can work, otherwise a plain list.
	choices := choicesOf(result, unattended)
	if len(choices) > 0 && !h.canOfferChoices(ctx, hosted) {
		content = withNumberedChoices(content, choices)
		choices = nil
	}

	// Deliver via streaming if the executor supports it, else fall back
	// to a single atomic send.
	if executor != nil {
		if sm, ok := executor.(domain.StreamingMessenger); ok {
			log.Printf("runtime: agent %q: streaming reply (%d runes)", hosted.Agent.ID, len([]rune(content)))
			chunks := chunkContent(ctx, content)
			messageID, _, serr := sm.StreamReply(ctx, identity, connRef, conv.ChatID, conv.ThreadID, chunks)
			if serr != nil {
				log.Printf("runtime: agent %q: stream reply failed, falling back: %v", hosted.Agent.ID, serr)
				messageID = h.deliverBestEffortID(ctx, hosted, conv, content)
			}
			h.attachChoices(ctx, hosted, conv, identity, connRef, executor, messageID, content, h.withDerived(ctx, hosted, result, userText, content, choices))
			if !unattended {
				h.maybeLearn(ctx, conv, hosted, userText, content)
			}
			return result, nil
		}
		log.Printf("runtime: agent %q: executor does not implement StreamingMessenger, using atomic send", hosted.Agent.ID)
	}
	messageID := h.deliverBestEffortID(ctx, hosted, conv, content)
	h.attachChoices(ctx, hosted, conv, identity, connRef, executor, messageID, content, h.withDerived(ctx, hosted, result, userText, content, choices))
	if !unattended {
		h.maybeLearn(ctx, conv, hosted, userText, content)
	}

	return result, nil
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
	var skillNames []string
	var intent domain.Intent
	var err error
	earlier := recentTurn(messages)

	// What kind of statement is this? Asked first and on its own, so that what happens next
	// does not depend on which abilities the agent has or how well they happen to match.
	classified := false
	if ic, ok := h.skillRouter.(IntentClassifier); ok {
		in, ierr := ic.ClassifyIntent(ctx, userText, earlier, scopeContext(hosted.Agent))
		if ierr != nil {
			log.Printf("skill-router: classify intent failed: %v — reading intent and abilities together", ierr)
		} else {
			intent, classified = in, true
			log.Printf("skill-router: intent=%q", intent)
			switch intent {
			case domain.IntentChat:
				log.Printf("skill-router: just being friendly — answering without tools")
				return h.chatReply(ctx, hosted, messages, actions)
			case domain.IntentOffTopic:
				if len(hosted.Agent.Skills) > 0 {
					log.Printf("skill-router: unrelated to this agent — replying with what I can do")
					h.noteCouldNot(ctx, hosted, userText)
					result, final := fixedReply(messages, declineText(hosted.Agent))
					return result, final, nil
				}
				log.Printf("skill-router: unrelated to this agent (no abilities) — letting LLM decline")
				h.noteCouldNot(ctx, hosted, userText)
				return h.respondDirectly(ctx, hosted, withDeclineHint(messages), actions)
			}
		}
	}

	var pickedIntent domain.Intent
	if tr, ok := h.skillRouter.(ToolAwareSkillRouter); ok && h.routeByTools {
		skillNames, pickedIntent, err = tr.PickSkillsWithTools(ctx, userText, earlier, h.routedSkills(hosted))
	} else if cr, ok := h.skillRouter.(ContextualSkillRouter); ok && !earlier.IsZero() {
		log.Printf("skill-router: reading the message next to the one before it")
		skillNames, pickedIntent, err = cr.PickSkillsInContext(ctx, userText, earlier, hosted.Agent.Skills)
	} else {
		skillNames, pickedIntent, err = h.skillRouter.PickSkills(ctx, userText, hosted.Agent.Skills)
	}
	if err != nil {
		log.Printf("skill-router: pick skills failed: %v — running NativeLoop with full actions", err)
		return h.runNativeLoop(ctx, hosted, messages, actions)
	}
	if !classified {
		intent = pickedIntent // no separate classifier: the picker's own reading stands
	}
	// Nobody is there to approve a change and nobody asked for it at this moment: an unattended turn
	// (a repeating task) reads and reports, whatever the stored words sound like. It is also never
	// the place to set up another repeating task.
	if domain.IsUnattended(ctx) && (intent == domain.IntentExecute || intent == domain.IntentSchedule) {
		if len(domain.UnattendedAllowed(ctx)) > 0 {
			// The person agreed to this task making changes, with these actions: it is a request
			// to DO something, held to it like any other (the change really happens or it says it didn't).
			intent = domain.IntentExecute
		} else {
			log.Printf("skill-router: unattended — %q is carried out as a look-up only", intent)
			intent = domain.IntentRecommend
		}
	} else if domain.IsUnattended(ctx) && intent == domain.IntentRecommend && len(domain.UnattendedAllowed(ctx)) > 0 {
		intent = domain.IntentExecute
	}

	log.Printf("skill-router: skills=%v intent=%q", skillNames, intent)

	// Only being sociable: answer like a person would, in their language, and nothing else.
	// No ability is run and none is remembered for a follow-up.
	if intent == domain.IntentChat {
		log.Printf("skill-router: just being friendly — answering without tools")
		return h.chatReply(ctx, hosted, messages, actions)
	}

	// A short follow-up that matches nothing on its own stays with what the conversation was
	// just doing; and whatever matched is remembered for the next one.
	carryConv, _ := domain.ConversationFromContext(ctx)
	carryID := carryKey(hosted.Agent.ID, carryConv)
	if len(skillNames) == 0 && isShortFollowUp(userText) {
		if prev := h.carry.recall(carryID); len(prev) > 0 {
			log.Printf("skill-router: nothing matched a short follow-up — staying with %v from the last message", prev)
			skillNames = prev
		}
	}
	h.carry.remember(carryID, skillNames)

	// Store intent on context so skills and prompt builders can read it.
	ctx = domain.WithIntent(ctx, intent)

	// Dispatch to a registered intent handler before the skill loop.
	// skillName is "" when no skill matched (e.g. "show my schedules").
	if handler, ok := h.intentHandlers[intent]; ok {
		if conv, ok := domain.ConversationFromContext(ctx); ok && conv.UserID != "" {
			skillName := ""
			if len(skillNames) > 0 {
				skillName = skillNames[0]
			}
			reply, herr := handler(ctx, conv.UserID, hosted.Agent.ID, skillName, userText, conv.IdentityID, conv.ChatID)
			if herr != nil {
				log.Printf("skill-router: intent handler %q: %v — falling through to skill loop", intent, herr)
			} else if strings.TrimSpace(reply) == "" {
				// The handler already said everything there was to say (it showed buttons, and the
				// answer or the lack of one is on screen): nothing more is sent.
				return &domain.LoopResult{Status: domain.LoopStatusComplete}, messages, nil
			} else {
				h.deliverBestEffort(ctx, hosted, conv, reply)
				// Keep the exchange, so "change it to 9am" has something to refer to. The
				// reply is already delivered, so the result itself stays silent.
				_, final := fixedReply(messages, reply)
				return &domain.LoopResult{Status: domain.LoopStatusComplete}, final, nil
			}
		}
	}

	if len(skillNames) == 0 {
		// If the agent has skills defined and none matched, the message is
		// outside this agent's scope — decline instead of letting the LLM
		// answer anything. Agents with no skills fall back to the full loop.
		if len(hosted.Agent.Skills) > 0 {
			// A question (not an off-topic one: that was settled above) that none of its
			// abilities covers, like "is the market open?" to a forex agent, is answered
			// plainly with no tools and no pretence of looking anything up. A request to DO
			// something it has no ability for is told what it can do instead.
			if classified && intent == domain.IntentRecommend {
				log.Printf("skill-router: a question no ability covers — answering without tools")
				result, final, rerr := h.respondDirectly(ctx, hosted, withHint(messages, inAreaHint), actions)
				if rerr != nil || !promisesLookup(result) {
					return result, withoutHint(final, inAreaHint), rerr
				}
				// It said it was about to check or run something, and with no tools it cannot: some
				// models ignore the instruction not to. Ask once more, and if it still does, say what
				// the agent can do instead of leaving a promise nothing will keep.
				log.Printf("skill-router: the reply promised a look-up it cannot do — asking again")
				retryHint := inAreaHint + " " + inAreaRetryHint
				result, final, rerr = h.respondDirectly(ctx, hosted, withHint(messages, retryHint), actions)
				if rerr != nil || promisesLookup(result) {
					log.Printf("skill-router: still promising a look-up — replying with what I can do")
					r, f := fixedReply(messages, declineText(hosted.Agent))
					return r, f, nil
				}
				return result, withoutHint(final, retryHint), rerr
			}
			log.Printf("skill-router: nothing it can do for this matched — replying with what I can do")
			h.noteCouldNot(ctx, hosted, userText)
			result, final := fixedReply(messages, declineText(hosted.Agent))
			return result, final, nil
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
			tools, err := h.hydrate(hosted, skill)
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

	// What the creator told these capabilities to do goes into the agent's own prompt for this message: measured on the
	// real model, instructions there are followed far more often than as a note beside the message (90% against 73%
	// on "do not use this" instructions). The prompt is not kept in the conversation, so neither are they.
	if instr := capabilityInstructionsHint(hosted.Agent.Skills, skillNames); instr != "" {
		messages = withSystemAddition(messages, instr)
	}
	return h.runRouted(ctx, hosted, userText, messages, actions, convActions, mergedTools, intent)
}

// withSystemAddition returns messages with text added to the end of the first system message, which is the agent's
// own prompt. The list the caller holds is not changed.
func withSystemAddition(messages []domain.Message, text string) []domain.Message {
	if len(messages) == 0 || messages[0].Role != domain.RoleSystem {
		return withHint(messages, text) // no prompt to add to: it rides beside the message instead
	}
	out := append([]domain.Message(nil), messages...)
	out[0].Content += "\n\n" + text
	return out
}

// runRouted runs a message with the tools of the capabilities routing chose.
func (h *Host) runRouted(ctx context.Context, hosted *HostedAgent, userText string, messages []domain.Message, actions, convActions, mergedTools []*domain.BoundAction, intent domain.Intent) (*domain.LoopResult, []domain.Message, error) {
	if len(mergedTools) == 0 {
		log.Printf("skill-router: selected skills yielded no tools — responding directly")
		return h.respondDirectly(ctx, hosted, messages, actions)
	}

	// An unattended run may look things up, and change only what the person agreed to.
	if domain.IsUnattended(ctx) {
		mergedTools = limitUnattended(mergedTools, domain.UnattendedAllowed(ctx))
		if len(mergedTools) == 0 {
			log.Printf("skill-router: unattended — nothing it may use here — responding directly")
			return h.respondDirectly(ctx, hosted, messages, actions)
		}
	}

	// For a request to DO something: which action(s) must really run for it to be done.
	allTools := mergedTools
	reqs := h.requirementsFor(ctx, userText, lastAssistantText(messages), intent, mergedTools)

	// Tool filter: trim irrelevant tools before the LLM call to reduce tokens.
	// Falls back to the full list on error or empty result.
	if h.toolFilter != nil {
		filtered, ferr := h.toolFilter.FilterTools(ctx, userText, mergedTools)
		if ferr != nil {
			log.Printf("skill-router: tool filter failed: %v — using full tool list", ferr)
		} else if len(filtered) > 0 {
			log.Printf("skill-router: tool filter: %d → %d tool(s)", len(mergedTools), len(filtered))
			mergedTools = filtered
		}
	}
	mergedTools = keepRequired(mergedTools, allTools, reqs)
	log.Printf("skill-router: offering tools %v", actionNames(mergedTools))

	// Recommend intent: filter to read-only tools so the LLM can fetch data
	// but not execute side-effecting actions. Inject a hint so it knows to
	// prefer advising over acting. If no read-only tools exist, respond without
	// tools — the LLM answers from knowledge.
	if intent == domain.IntentRecommend {
		var readTools []*domain.BoundAction
		for _, t := range mergedTools {
			if t.Spec.ReadOnly {
				readTools = append(readTools, t)
			}
		}
		if len(readTools) == 0 {
			log.Printf("skill-router: recommend intent — no read-only tools, responding directly")
			return h.respondDirectly(ctx, hosted, withRecommendHint(messages), actions)
		}
		log.Printf("skill-router: recommend intent — running with %d read-only tool(s)", len(readTools))
		messages = withRecommendHint(messages)
		mergedTools = readTools
	}

	loopActions := make([]*domain.BoundAction, 0, len(mergedTools)+len(convActions)+1)
	loopActions = append(loopActions, mergedTools...)
	loopActions = append(loopActions, convActions...)
	loopActions = append(loopActions, h.finishFor(ctx, hosted, reqs != nil))
	if reqs != nil {
		ctx = domain.WithRequirements(ctx, reqs)
	}
	return h.runNativeLoop(ctx, hosted, messages, loopActions)
}

// maxCapabilityInstructions bounds what the creator's capability instructions may add to one message.
const maxCapabilityInstructions = 4000

// capabilityInstructionsHint gathers the instructions the creator wrote for the capabilities routing chose. They are
// the creator's own words about how to do the job, so they come with the message; they are framed as following the
// platform's rules, never above them. "" when none of the chosen capabilities has any.
func capabilityInstructionsHint(all []*domain.Skill, chosen []string) string {
	want := make(map[string]bool, len(chosen))
	for _, n := range chosen {
		want[n] = true
	}
	var parts []string
	for _, sk := range all {
		if !want[sk.Name] {
			continue
		}
		if text := strings.TrimSpace(sk.Instructions); text != "" {
			parts = append(parts, "For \""+domain.SkillLabel(sk)+"\":\n"+text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	body := strings.Join(parts, "\n\n")
	if r := []rune(body); len(r) > maxCapabilityInstructions {
		body = string(r[:maxCapabilityInstructions])
	}
	return "The creator of this assistant gave instructions for what you are doing now. Follow them closely: they decide how you do this job, " +
		"including when to use an ability and when not to, and what to say. Where they say not to use something, do not use it. " +
		"Only your other rules come before them: ask before changing anything, never say you did something you did not do, " +
		"and keep each person's information private. Their instructions:\n\n" + body
}

// limitUnattended keeps what an unattended run may use: every look-up, and only the actions named
// in allowed for changing things. Anything else that could change something (or is unknown) goes.
func limitUnattended(tools []*domain.BoundAction, allowed []string) []*domain.BoundAction {
	ok := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		ok[n] = true
	}
	var out []*domain.BoundAction
	for _, t := range tools {
		if t.Spec.ReadOnly || ok[t.Spec.Name] {
			out = append(out, t)
		}
	}
	return out
}

// withRecommendHint prepends a system-level directive so the LLM knows the
// user wants advice, not action — it should answer directly without invoking
// action tools.
func withRecommendHint(messages []domain.Message) []domain.Message {
	const hint = "The user wants advice or information, not to take an action right now. " +
		"Answer directly from your knowledge and capabilities. " +
		"Do not invoke tools to execute actions on the user's behalf."
	out := make([]domain.Message, 0, len(messages)+1)
	if len(messages) > 0 {
		out = append(out, messages[:len(messages)-1]...)
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
		out = append(out, messages[len(messages)-1])
	} else {
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
	}
	return out
}

// withDeclineHint prepends a system-level directive to the message list so
// the LLM knows it must decline the user's request instead of answering it.
// Inserted as a second system message right before the user's message so it
// takes precedence over the general system prompt without replacing it.
// chatHint tells the model what a purely social message needs: warmth, brevity, their
// language, and no pretence of doing anything.
const chatHint = "The person is only being friendly: a greeting, thanks, a goodbye or small talk. " +
	"Reply warmly in one or two short sentences, in the language they write in. " +
	"You may say in a few words what you can help with. " +
	"Do not say or imply that you are looking something up, checking, running or doing anything, and do not offer anything you cannot do."

// chatReply answers a purely social message. The model has only the ability to reply, so it
// cannot act; the instruction that shaped the reply is not kept in the conversation.
func (h *Host) chatReply(ctx context.Context, hosted *HostedAgent, messages []domain.Message, actions []*domain.BoundAction) (*domain.LoopResult, []domain.Message, error) {
	result, final, err := h.respondDirectly(ctx, hosted, withHint(messages, chatHint), actions)
	return result, withoutHint(final, chatHint), err
}

// withHint inserts an instruction as a system message right before the person's message.
func withHint(messages []domain.Message, hint string) []domain.Message {
	out := make([]domain.Message, 0, len(messages)+1)
	if len(messages) > 0 {
		out = append(out, messages[:len(messages)-1]...)
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
		out = append(out, messages[len(messages)-1])
	} else {
		out = append(out, domain.Message{Role: domain.RoleSystem, Content: hint})
	}
	return out
}

// withoutHint removes an instruction that was only there to shape one reply, so it is not
// stored as part of the conversation.
func withoutHint(messages []domain.Message, hint string) []domain.Message {
	out := make([]domain.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == domain.RoleSystem && m.Content == hint {
			continue
		}
		out = append(out, m)
	}
	return out
}

// inAreaHint tells the model what a question in its own area, that none of its abilities
// covers, needs: a short honest answer, and no pretence of looking anything up or doing anything.
const inAreaHint = "The person asked about something in your area that none of your abilities covers. " +
	"Answer in a sentence or two from general knowledge if you can, in the language they write in. " +
	"Never say or imply that you are looking something up, checking, running or doing anything. " +
	"Do not state live prices, quotes or other current data that you do not have, and do not promise anything you cannot do. " +
	"If you cannot answer without live data or an ability you do not have, say so plainly and say briefly what you can do."

// inAreaRetryHint is added when a reply to such a question promised a look-up that cannot happen.
const inAreaRetryHint = "Your last attempt said you were about to look something up, check or run something. You cannot do that here, so do not say it. " +
	"Answer from what you know in a sentence or two, or say plainly what you can do and what you would need from them."

// promiseOfLookup finds a reply that says it is about to look something up or run something.
var promiseOfLookup = regexp.MustCompile(`(?i)(let me (check|look|scan|run|pull|see|find|analy[sz]e|search|fetch|get|go)\b|i'?ll (check|look|scan|run|pull|find|analy[sz]e|search|fetch|get)\b|i will (check|look|scan|run|pull|find|analy[sz]e|search|fetch|get)\b|i('m| am) (checking|looking|scanning|running|pulling|analy[sz]ing|searching)\b|one moment|give me a (moment|second|sec)\b)`)

// promisesLookup reports whether a turn's reply promises a look-up (in its first few sentences).
func promisesLookup(result *domain.LoopResult) bool {
	if result == nil || result.Content == "" {
		return false
	}
	r := []rune(result.Content)
	if len(r) > 240 {
		r = r[:240]
	}
	return promiseOfLookup.MatchString(string(r))
}

// scopeContext describes what an agent is for, so a router can judge whether a message is in
// its area: what the creator wrote about it. The list of abilities is deliberately left out:
// measured on the real router, it reads "in scope" as "one of these abilities" and turns away
// ordinary questions in the same field (a bank's support agent scored 0.04 on "what are your
// opening hours"), while the description alone separates in-area (0.92 and up) from off-topic
// (0.20 and under) cleanly.
func scopeContext(agent *domain.Agent) string {
	about := strings.TrimSpace(agent.Description)
	if about == "" {
		about = strings.TrimSpace(agent.Instructions)
		if len(about) > 500 {
			about = about[:500]
		}
	}
	return about
}

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
func (h *Host) onboardingMessage(agent *domain.Agent, linkCode string) (text, linkURL string) {
	body := greetingBody(agent)

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

// greetingBody is the first thing a new person is told. The creator's own greeting
// is used word for word; without one, a fixed introduction is built from the agent's
// name, description and what it can do. No model is involved, so every person gets
// the same welcome.
func greetingBody(agent *domain.Agent) string {
	if agent.Greeting != "" {
		return agent.Greeting
	}
	var b strings.Builder
	b.WriteString("👋 Hi! I'm " + agent.Name + ".")
	if agent.Description != "" {
		b.WriteString("\n\n" + agent.Description)
	}
	if abilities := welcomeAbilities(agent); len(abilities) > 0 {
		b.WriteString("\n\nHere's what I can help with:")
		for _, line := range abilities {
			b.WriteString("\n• " + line)
		}
	}
	return b.String()
}

// welcomeAbilities lists what the agent can do, one plain line each, in the words its
// creator gave (or the name, tidied, when they gave none). It covers everything the
// agent does for the person, whether or not other agents may call it.
func welcomeAbilities(agent *domain.Agent) []string {
	var out []string
	for _, s := range agent.Skills {
		line := strings.TrimSpace(s.Description)
		if line == "" {
			line = skillDisplayName(s.Name)
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
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

// connectPromptCooldown is how long a person is left alone after being sent a
// connect prompt for an identity. A model often calls several tools at once; each
// would otherwise send its own copy of the same prompt. After the cooldown a new
// request sends a fresh prompt, so someone who ignored the first isn't stuck.
const connectPromptCooldown = 30 * time.Second

// connectPromptNow is the clock for the cooldown (replaced in tests).
var connectPromptNow = time.Now

// shouldSendConnectPrompt reports whether this person should be sent a connect
// prompt for the identity now, and records it if so. It is safe to call from many
// goroutines: exactly one of several simultaneous callers gets true.
func (h *Host) shouldSendConnectPrompt(key string) bool {
	h.connectPromptMu.Lock()
	defer h.connectPromptMu.Unlock()
	now := connectPromptNow()
	if last, ok := h.connectPromptSent[key]; ok && now.Sub(last) < connectPromptCooldown {
		return false
	}
	if h.connectPromptSent == nil {
		h.connectPromptSent = make(map[string]time.Time)
	}
	for k, t := range h.connectPromptSent { // forget old entries so the table can't grow
		if now.Sub(t) >= connectPromptCooldown {
			delete(h.connectPromptSent, k)
		}
	}
	h.connectPromptSent[key] = now
	return true
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
		key := userID + ":" + identity.ID

		// Store the turn for replay once the user connects. The original user
		// message comes from context (set by listen.go before HandleTurn). This
		// happens every time, even when the prompt itself is not repeated.
		storePending := func() {
			userMessage := domain.OriginalMessageFromContext(ctx)
			if userMessage != "" {
				h.holdTurn(ctx, userID, identity.ID, pendingAuthTurn{
					conv:        conv,
					agentID:     hosted.Agent.ID,
					userMessage: userMessage,
				})
				log.Printf("runtime: auth gate: stored pending turn key=%q message=%q", key, userMessage)
			} else {
				log.Printf("runtime: auth gate: no original message in context — turn will NOT be replayed (key=%q)", key)
			}
		}

		// One prompt per person per moment: a model that calls three tools at once
		// would otherwise send the same connect prompt three times.
		if !h.shouldSendConnectPrompt(key) {
			storePending()
			return "", nil
		}

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

		h.noteEvent(hosted, domain.ActivitySignIn, integrationName)
		promptText, buttonLabel := connectPrompt(integrationName)
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
		messageID := h.deliverBestEffortID(ctx, hosted, conv, promptText, extra)
		if messageID != "" && h.connectPromptHook != nil {
			h.connectPromptHook(ctx, ConnectPrompt{UserID: userID, IdentityID: identity.ID, Conv: conv, MessageID: messageID, Text: promptText})
		}

		storePending()

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
	h.deliverBestEffortID(ctx, hosted, conv, text, extra...)
}

// deliverBestEffortID is deliverBestEffort that also returns the sent
// message's ID when the messenger reports one ("" otherwise) — needed to edit
// the message later, e.g. to retire a Connect button once it has been used.
func (h *Host) deliverBestEffortID(ctx context.Context, hosted *HostedAgent, conv domain.ConversationRef, text string, extra ...map[string]any) string {
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		log.Printf("runtime: agent %q: no integration for provider %q, cannot deliver reply", hosted.Agent.ID, conv.Provider)
		return ""
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		log.Printf("runtime: agent %q: no executor for service %q, cannot deliver reply", hosted.Agent.ID, integration.Service)
		return ""
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
	out, err := executor.Execute(ctx, identity, connectionRef, domain.ActionSendMessage, input)
	if err != nil {
		log.Printf("runtime: agent %q: failed to deliver reply: %v", hosted.Agent.ID, err)
		return ""
	}
	if m, ok := out.(map[string]any); ok {
		if id, ok := m["message_id"].(string); ok {
			return id
		}
	}
	return ""
}

// RunScheduledTask carries out a repeating task the person set up: task is what they asked for
// ("check my calendar and send me a summary"), run now as if they had just written it, with
// everything a normal message gets (their details, the right capability, the agent's rules) except
// that nobody is there: it can look things up and report, ask no question and wait for no approval, and
// change only what allowChanges names (what the person agreed to when they set the task up; nothing
// by default). See domain.WithUnattendedAllowing. The reply goes to (identityID, channelRef).
// It runs in the background and returns at once.
func (h *Host) RunScheduledTask(ctx context.Context, agentID, userID, identityID, channelRef, task string, allowChanges ...string) error {
	h.mu.RLock()
	hosted, ok := h.agents[agentID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("RunScheduledTask: agent %q not registered", agentID)
	}
	task = strings.TrimSpace(task)
	if task == "" {
		return fmt.Errorf("RunScheduledTask: no task to run")
	}
	conv := domain.ConversationRef{IdentityID: identityID, ChatID: channelRef, UserID: userID, SenderID: channelRef}
	if identity, integration := resolveIdentityAndIntegration(hosted, conv); identity != nil && integration != nil {
		conv.Provider = integration.Service
	}
	ctx = domain.WithUnattendedAllowing(ctx, allowChanges)

	go func() {
		defer recoverFromPanic(hosted.Agent.ID, "repeating task")
		// HandleTurn loads the person's connections and details like any turn, and tells them
		// plainly if it couldn't finish.
		if _, err := h.HandleTurn(ctx, hosted, conv, task); err != nil {
			log.Printf("runtime: repeating task agent %q user %q: %v", agentID, userID, err)
			return
		}
		log.Printf("runtime: repeating task agent %q user %q: finished", agentID, userID)
	}()
	return nil
}
