package domain

const (
	// AppAuthorizationScopeUser is a per-user authorization — one record per
	// user who authorized the app (GitHub OAuth, Google OAuth, etc.).
	AppAuthorizationScopeUser = "user"

	// AppAuthorizationScopeWorkspace is a workspace/org/server-level
	// authorization — one record covers all users in that workspace
	// (Slack workspace install, Discord server install, MS Teams org install).
	AppAuthorizationScopeWorkspace = "workspace"
)

// AppAuthorization records that a user (or workspace admin) authorized a
// specific Identity to act on their behalf. It is the user-side counterpart
// to Identity (the creator-side app registration).
//
// Created exclusively via OAuth flows or app-install flows — never by users
// pasting raw tokens. CredentialRef is an opaque pointer to the encrypted
// credential (OAuth access token, installation ID, workspace bot token, etc.).
//
// Scope distinguishes per-user authorizations (GitHub, Google) from
// workspace-level installs (Slack, Discord, Teams) where one authorization
// covers all users in a workspace.
type AppAuthorization struct {
	ID            string `json:"id"`
	IdentityID    string `json:"identity_id"`
	UserID        string `json:"user_id"`
	Scope         string `json:"scope"` // AppAuthorizationScopeUser | AppAuthorizationScopeWorkspace
	Name          string `json:"name"`  // human-readable label, e.g. "github/charlesdinneya"
	CredentialRef string `json:"credential_ref"`
	// ExternalUserID is the provider's own identifier for the user — e.g. the
	// GitHub login "octocat". Stored during OAuth so event payloads that carry
	// a sender can be matched back to the internal user who connected that account.
	ExternalUserID string `json:"external_user_id,omitempty"`
}
