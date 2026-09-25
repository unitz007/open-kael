package api_test

import (
	"context"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// noopStore is a domain.Store where every method returns a zero value and
// nil error — useful as a base for test stubs that only need a subset of
// the interface to exercise a specific behavior (e.g. auth middleware).
type noopStore struct{}

func (noopStore) SaveUser(_ context.Context, _ *domain.User) error                { return nil }
func (noopStore) GetUser(_ context.Context, _ string) (*domain.User, error)       { return nil, domain.ErrNotFound }
func (noopStore) GetUserByEmail(_ context.Context, _ string) (*domain.User, error) { return nil, domain.ErrNotFound }
func (noopStore) DeleteUser(_ context.Context, _ string) error                    { return nil }

func (noopStore) SaveSession(_ context.Context, _ *domain.Session) error { return nil }
func (noopStore) GetSession(_ context.Context, _ string) (*domain.Session, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) DeleteSession(_ context.Context, _ string) error { return nil }

func (noopStore) SaveAppAuthorization(_ context.Context, _ *domain.AppAuthorization) error { return nil }
func (noopStore) GetAppAuthorization(_ context.Context, _ string) (*domain.AppAuthorization, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) GetAppAuthorizationByExternalUser(_ context.Context, _, _ string) (*domain.AppAuthorization, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) GetAppAuthorizationByUserAndIdentity(_ context.Context, _, _ string) (*domain.AppAuthorization, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) ListAppAuthorizationsByUser(_ context.Context, _ string) ([]*domain.AppAuthorization, error) {
	return nil, nil
}
func (noopStore) DeleteAppAuthorization(_ context.Context, _ string) error { return nil }

func (noopStore) SaveMessengerChannel(_ context.Context, _ *domain.MessengerChannel) error { return nil }
func (noopStore) GetMessengerChannelByIdentityAndRef(_ context.Context, _, _ string) (*domain.MessengerChannel, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) ListMessengerChannelsByUser(_ context.Context, _ string) ([]*domain.MessengerChannel, error) {
	return nil, nil
}
func (noopStore) DeleteMessengerChannel(_ context.Context, _ string) error { return nil }

func (noopStore) SaveChannelLinkCode(_ context.Context, _ *domain.ChannelLinkCode) error {
	return nil
}
func (noopStore) GetChannelLinkCode(_ context.Context, _ string) (*domain.ChannelLinkCode, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) DeleteChannelLinkCode(_ context.Context, _ string) error { return nil }

func (noopStore) SaveIdentity(_ context.Context, _ *domain.Identity) error { return nil }
func (noopStore) ListIdentities(_ context.Context) ([]*domain.Identity, error) {
	return nil, nil
}
func (noopStore) ListIdentitiesByIntegration(_ context.Context, _ string) ([]*domain.Identity, error) {
	return nil, nil
}
func (noopStore) GetIdentity(_ context.Context, _ string) (*domain.Identity, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) DeleteIdentity(_ context.Context, _ string) error { return nil }

func (noopStore) SaveAgent(_ context.Context, _ *domain.Agent) error { return nil }
func (noopStore) ListAgents(_ context.Context, _ string) ([]*domain.Agent, error) {
	return nil, nil
}
func (noopStore) GetAgent(_ context.Context, _ string) (*domain.Agent, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) GetAgentByIdentityID(_ context.Context, _ string) (*domain.Agent, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) LoadAgent(_ context.Context, _ string) (*domain.Agent, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) DeleteAgent(_ context.Context, _ string) error { return nil }

func (noopStore) ListIntegrations(_ context.Context) ([]*domain.Integration, error) { return nil, nil }
func (noopStore) LoadIntegration(_ context.Context, _ string) (*domain.Integration, error) {
	return nil, domain.ErrNotFound
}

func (noopStore) GetTool(_ context.Context, _ string) (*domain.ToolDefinition, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) ListToolsByIntegration(_ context.Context, _ string) ([]*domain.ToolDefinition, error) {
	return nil, nil
}

func (noopStore) SaveSkill(_ context.Context, _ *domain.Skill) error { return nil }
func (noopStore) GetSkill(_ context.Context, _ string) (*domain.Skill, error) {
	return nil, domain.ErrNotFound
}
func (noopStore) ListSkillsByAgent(_ context.Context, _ string) ([]*domain.Skill, error) {
	return nil, nil
}
func (noopStore) DeleteSkill(_ context.Context, _ string) error { return nil }

func (noopStore) LoadAll(_ context.Context) ([]*domain.Agent, map[string]*domain.ToolDefinition, map[string]*domain.Identity, map[string]*domain.Integration, error) {
	return nil, nil, nil, nil, nil
}

// stubStoreWithUser embeds noopStore but returns a valid session for the
// "valid-token" bearer token, pointing at a fixed user "u-stub-1". Useful for
// testing authenticated endpoints without a real database.
type stubStoreWithUser struct{ noopStore }

func (s *stubStoreWithUser) GetSession(_ context.Context, token string) (*domain.Session, error) {
	if token == "valid-token" {
		return &domain.Session{Token: token, UserID: "u-stub-1", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	return nil, domain.ErrNotFound
}

func (s *stubStoreWithUser) GetUser(_ context.Context, id string) (*domain.User, error) {
	if id == "u-stub-1" {
		return &domain.User{ID: id, Email: "stub@example.com"}, nil
	}
	return nil, domain.ErrNotFound
}
