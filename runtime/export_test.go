package runtime

import (
	"context"

	"github.com/unitz007/open-kael/domain"
)

// SetProposalDispatch lets a test see what a tapped choice is handed to the conversation as.
func (h *Host) SetProposalDispatch(f func(context.Context, *HostedAgent, domain.InboundMessage)) {
	h.proposalDispatch = f
}

// HandleInbound lets a test hand a message to the agent as a chat app would.
func (h *Host) HandleInbound(ctx context.Context, hosted *HostedAgent, msg domain.InboundMessage) {
	h.handleInboundSafely(ctx, hosted, msg)
}
