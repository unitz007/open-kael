package api_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
)

func TestIntegrationAccessible(t *testing.T) {
	platform := &domain.Integration{ID: "p"}
	mine := &domain.Integration{ID: "m", CreatedBy: "u1"}
	theirs := &domain.Integration{ID: "t", CreatedBy: "u2"}

	u1 := api.ContextWithAccount(context.Background(), &domain.Account{ID: "u1"})
	for _, tc := range []struct {
		name string
		i    *domain.Integration
		want bool
	}{{"platform", platform, true}, {"own", mine, true}, {"someone else's", theirs, false}} {
		if got := api.IntegrationAccessible(u1, tc.i); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// No session (static-token / dev mode) keeps seeing everything, like identities.
	if !api.IntegrationAccessible(context.Background(), theirs) {
		t.Error("no-session mode should see all integrations")
	}
}
