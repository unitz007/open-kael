package domain

// Identity kind constants describe the auth mechanism — not the provider.
const (
	// IdentityKindToken is a raw API key or personal access token stored
	// directly on the Identity. Used only for creator-level service accounts,
	// never for user credentials (users connect via OAuth/app install flows).
	IdentityKindToken = "token"

	// IdentityKindBot is a registered app or bot account acting as itself.
	// Examples: Telegram bot, Slack app, Discord bot, GitHub App.
	// CredentialRef resolves to the app credential — a bot token for most
	// providers; for GitHub Apps an RSA private key (PEM) used to mint a
	// short-lived JWT and exchange it for an installation access token
	// (AppID carries the numeric GitHub App ID for the JWT iss claim).
	IdentityKindBot = "bot"

	// IdentityKindOAuth is an OAuth application registration. The Identity
	// carries the app's client credentials. Users authorize it via the OAuth
	// flow which creates an AppAuthorization with their access token —
	// the executor uses the AppAuthorization credential, not the Identity's.
	IdentityKindOAuth = "oauth"
)

// Identity is one app/bot/OAuth-app credential registered with a provider —
// the "GitHub App", "Telegram Bot", or "OAuth App" the creator set up.
// It lives inside an Integration (IntegrationID) which groups all identities
// and tools for a given service.
//
// An Integration can have multiple Identities for the same service (e.g. two
// GitHub Apps targeting different orgs). Agents declare which specific
// Identity IDs they use via Agent.IdentityIDs.
//
// CredentialRef is an opaque pointer into a secure credential store — no raw
// secret ever lives on this struct.
type Identity struct {
	ID            string `json:"id"`
	IntegrationID string `json:"integration_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	// AppID is the provider-assigned numeric app identifier needed for
	// certain auth flows (e.g. GitHub App JWT iss claim). Empty otherwise.
	AppID         string `json:"app_id,omitempty"`
	CredentialRef string `json:"credential_ref"`
	// WebhookSecretRef is an encrypted reference to the webhook secret for
	// this identity, populated only for integrations that receive webhooks
	// (e.g. GitHub App). Empty for identities that don't use webhooks.
	WebhookSecretRef string `json:"webhook_secret_ref,omitempty"`
}
