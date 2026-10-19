package postgres_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A creator's own connection keeps its owner, its source description and the
// per-tool Off switch across a save/load.
func TestCustomIntegrationRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.SaveAccount(ctx, &domain.Account{ID: "u_creator", Email: "c@example.com"}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteAccount(context.Background(), "u_creator") })

	in := &domain.Integration{
		ID: "custom_1", Name: "Pet shop", Service: "custom_1", Kind: domain.IntegrationKindUser,
		CreatedBy: "u_creator",
		Source: &domain.IntegrationSource{
			Type: domain.SourceOpenAPI, URL: "https://example.com/spec.json", BaseURL: "https://api.example.com/v2",
			Auth: domain.SourceAuth{Kind: domain.AuthHeader, Name: "api_key"},
		},
	}
	if err := store.SaveIntegration(ctx, in); err != nil {
		t.Fatalf("save integration: %v", err)
	}
	platform := &domain.Integration{ID: "plat_1", Name: "Platform", Service: "plat", Kind: domain.IntegrationKindSystem}
	if err := store.SaveIntegration(ctx, platform); err != nil {
		t.Fatalf("save platform integration: %v", err)
	}

	got, err := store.LoadIntegration(ctx, "custom_1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.CreatedBy != "u_creator" || got.Source == nil || got.Source.Type != domain.SourceOpenAPI ||
		got.Source.BaseURL != "https://api.example.com/v2" || got.Source.Auth.Kind != domain.AuthHeader || got.Source.Auth.Name != "api_key" {
		t.Fatalf("owner/source did not round-trip: %+v %+v", got, got.Source)
	}

	plat, err := store.LoadIntegration(ctx, "plat_1")
	if err != nil {
		t.Fatalf("load platform: %v", err)
	}
	if plat.CreatedBy != "" || plat.Source != nil {
		t.Fatalf("platform integration must stay ownerless and source-less: %+v", plat)
	}

	if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "t_off", Name: "t_off", IntegrationID: "custom_1", Disabled: true}); err != nil {
		t.Fatalf("save tool: %v", err)
	}
	if err := store.SaveTool(ctx, &domain.ToolDefinition{ID: "t_on", Name: "t_on", IntegrationID: "custom_1"}); err != nil {
		t.Fatalf("save tool: %v", err)
	}
	off, err := store.GetTool(ctx, "t_off")
	if err != nil || !off.Disabled {
		t.Fatalf("Disabled did not round-trip: %+v err=%v", off, err)
	}
	on, err := store.GetTool(ctx, "t_on")
	if err != nil || on.Disabled {
		t.Fatalf("enabled tool came back disabled: %+v err=%v", on, err)
	}
}
