package domain

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Store methods when a requested record does not
// exist. Callers check with errors.Is to distinguish 404 from 500.
var ErrNotFound = errors.New("not found")

// Store is the persistence contract for the framework's domain model.
// Any backing store that implements every method below can be passed to
// api.NewServer or used as a runtime.AgentGraphLoader without importing
// a concrete package. The postgres package in this repo is one such
// implementation; it is not special.
//
// Implementations must return ErrNotFound (wrapped or unwrapped) when a
// single-record lookup finds no row.
type Store interface {
	// Integration reads — platform-seeded at startup, read-only for creators.
	ListIntegrations(ctx context.Context) ([]*Integration, error)
	LoadIntegration(ctx context.Context, id string) (*Integration, error)

	// Identity CRUD — app/bot credentials belonging to an Integration.
	SaveIdentity(ctx context.Context, i *Identity) error
	ListIdentities(ctx context.Context) ([]*Identity, error)
	ListIdentitiesByIntegration(ctx context.Context, integrationID string) ([]*Identity, error)
	GetIdentity(ctx context.Context, id string) (*Identity, error)
	DeleteIdentity(ctx context.Context, id string) error

	// Tool reads — platform-seeded at startup, read-only for creators.
	GetTool(ctx context.Context, id string) (*ToolDefinition, error)
	ListToolsByIntegration(ctx context.Context, integrationID string) ([]*ToolDefinition, error)

	// Agent CRUD
	SaveAgent(ctx context.Context, a *Agent) error
	ListAgents(ctx context.Context, ownerID string) ([]*Agent, error)
	GetAgent(ctx context.Context, id string) (*Agent, error)
	GetAgentByIdentityID(ctx context.Context, identityID string) (*Agent, error)
	LoadAgent(ctx context.Context, id string) (*Agent, error)
	DeleteAgent(ctx context.Context, id string) error

	// Skill CRUD
	SaveSkill(ctx context.Context, skill *Skill) error
	GetSkill(ctx context.Context, id string) (*Skill, error)
	ListSkillsByAgent(ctx context.Context, agentID string) ([]*Skill, error)
	DeleteSkill(ctx context.Context, id string) error

	// User CRUD
	SaveUser(ctx context.Context, u *User) error
	GetUser(ctx context.Context, id string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByVerificationToken(ctx context.Context, token string) (*User, error)
	DeleteUser(ctx context.Context, id string) error

	// Session CRUD
	SaveSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, token string) (*Session, error)
	DeleteSession(ctx context.Context, token string) error

	// AppAuthorization CRUD — user's auth grant for an API provider,
	// created via OAuth flow or app-install flow only.
	SaveAppAuthorization(ctx context.Context, a *AppAuthorization) error
	GetAppAuthorization(ctx context.Context, id string) (*AppAuthorization, error)
	GetAppAuthorizationByUserAndIdentity(ctx context.Context, userID, identityID string) (*AppAuthorization, error)
	// GetAppAuthorizationByExternalUser looks up the AppAuthorization where
	// identity_id = identityID and external_user_id = externalUserID. Used by
	// the event runner to map a webhook sender back to the internal user.
	GetAppAuthorizationByExternalUser(ctx context.Context, identityID, externalUserID string) (*AppAuthorization, error)
	GetAppAuthorizationByCredentialRef(ctx context.Context, credentialRef string) (*AppAuthorization, error)
	ListAppAuthorizationsByUser(ctx context.Context, userID string) ([]*AppAuthorization, error)
	DeleteAppAuthorization(ctx context.Context, id string) error

	// MessengerChannel CRUD — user's messaging address on a bot Identity.
	SaveMessengerChannel(ctx context.Context, ch *MessengerChannel) error
	GetMessengerChannelByIdentityAndRef(ctx context.Context, identityID, channelRef string) (*MessengerChannel, error)
	ListMessengerChannelsByUser(ctx context.Context, userID string) ([]*MessengerChannel, error)
	DeleteMessengerChannel(ctx context.Context, id string) error

	// ChannelLinkCode CRUD — short-lived tokens for linking a messenger
	// identity to an existing platform account.
	SaveChannelLinkCode(ctx context.Context, c *ChannelLinkCode) error
	GetChannelLinkCode(ctx context.Context, code string) (*ChannelLinkCode, error)
	DeleteChannelLinkCode(ctx context.Context, code string) error

	// UserToolApproval — per-user gate preferences.
	// SetUserToolApproval adds (requires=true) or removes (requires=false) the
	// user's approval gate for toolID. ListUserToolApprovals returns the current
	// set as a map[toolID]bool ready for domain.WithUserToolApprovals.
	SetUserToolApproval(ctx context.Context, userID, toolID string, requires bool) error
	ListUserToolApprovals(ctx context.Context, userID string) (map[string]bool, error)

	// UserAgentConfig — explicit per-agent instructions set by the user (via /instructions).
	// GetUserAgentConfig returns ErrNotFound when no config exists yet.
	GetUserAgentConfig(ctx context.Context, userID, agentID string) (*UserAgentConfig, error)
	SetUserAgentConfig(ctx context.Context, cfg *UserAgentConfig) error

	// UserProfile — general facts about the user learned by the agent over time,
	// shared across all agents. GetUserProfile returns ErrNotFound when none exists.
	GetUserProfile(ctx context.Context, userID string) (*UserProfile, error)
	SetUserProfile(ctx context.Context, userID, notes string) error

	// UserIntegrationNotes — integration-specific facts learned per user.
	// ListUserIntegrationNotesByUser returns all notes for a user as integrationID → notes.
	// SetUserIntegrationNotes upserts notes for one (user, integration) pair.
	ListUserIntegrationNotesByUser(ctx context.Context, userID string) (map[string]string, error)
	SetUserIntegrationNotes(ctx context.Context, userID, integrationID, notes string) error

	// MarkMessengerChannelOnboardingPrompted stamps onboarding_prompted_at = now()
	// so the pending onboarding state survives server restarts.
	MarkMessengerChannelOnboardingPrompted(ctx context.Context, identityID, channelRef string) error

	// MarkMessengerChannelOnboarded stamps onboarded_at = now() on the
	// messenger_channel row identified by (identityID, channelRef).
	MarkMessengerChannelOnboarded(ctx context.Context, identityID, channelRef string) error

	// PlatformConfig — generic key/value store for platform-level settings that
	// must persist across restarts (e.g. rotating OAuth refresh tokens).
	// GetPlatformConfig returns ErrNotFound when the key has not been set.
	GetPlatformConfig(ctx context.Context, key string) (string, error)
	SetPlatformConfig(ctx context.Context, key, value string) error

	// LoadAll is the boot-time bulk loader: returns every Agent (with Skills),
	// every ToolDefinition keyed by ID, every Identity keyed by ID, and every
	// Integration keyed by ID (with its Identities and Tools populated).
	LoadAll(ctx context.Context) (agents []*Agent, toolsByID map[string]*ToolDefinition, identitiesByID map[string]*Identity, integrationsByID map[string]*Integration, err error)
}
