package domain

type IntegrationStatus string

const (
	IntegrationStatusConnected    IntegrationStatus = "connected"
	IntegrationStatusDisconnected IntegrationStatus = "disconnected"
	IntegrationStatusError        IntegrationStatus = "error"
)

// Integration represents one connected external service/account (Slack,
// Google, GitHub, FPL, or a "local" provider standing in for an existing
// in-process tool). ConnectionRef is an opaque pointer into wherever real
// credentials live — mirroring existing practice, where no credential has
// ever lived on a tool. Tools is a convenience, denormalized view for
// API/UI consumption; ToolDefinition.IntegrationID is the source of truth.
//
// OwnerID identifies who a connection belongs to — this model was
// originally built for a multi-tenant platform, so a connected account is
// always someone's, never global, even where (as in Kael today) there is
// only one owner. There is deliberately no User/Account type — OwnerID is
// just an opaque caller-supplied identifier, threaded through end-to-end so
// the shape is correct ahead of any real multi-tenant use, not enforced by
// anything today.
type Integration struct {
	ID          string `json:"id"`
	OwnerID     string `json:"owner_id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Description string `json:"description"`

	ConnectionRef string            `json:"connection_ref"`
	Status        IntegrationStatus `json:"status"`

	Tools []*ToolDefinition `json:"tools"`
}
