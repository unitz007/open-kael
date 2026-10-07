package runtime

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

// connectRequesterFunc adapts a plain function to domain.ConnectRequester —
// the http.HandlerFunc pattern, used by resolveConnectRequester in host.go to
// bind the agent, conversation, and URL generator before exposing the requester
// to domain.HydrateTool's auth gate without the gate ever knowing the host.
type connectRequesterFunc func(ctx context.Context, conv domain.ConversationRef, identity *domain.Identity, userID string) (string, error)

func (f connectRequesterFunc) RequestConnect(ctx context.Context, conv domain.ConversationRef, identity *domain.Identity, userID string) (string, error) {
	return f(ctx, conv, identity, userID)
}
