package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// approvalRequesterFunc adapts a plain function to domain.ApprovalRequester
// — the http.HandlerFunc pattern, used by resolveApprovalRequester to bind
// a TenantApprovalRequester's Integration once per turn without hydrate.go
// (the actual caller, via domain.HydrateTool's approval gate) ever needing
// to know multi-tenancy exists.
type approvalRequesterFunc func(ctx context.Context, conv domain.ConversationRef, prompt string, timeoutSeconds int) (bool, error)

func (f approvalRequesterFunc) RequestApproval(ctx context.Context, conv domain.ConversationRef, prompt string, timeoutSeconds int) (bool, error) {
	return f(ctx, conv, prompt, timeoutSeconds)
}

// resolveApprovalRequester finds conv's bot Identity/Executor and, if that
// Executor can request approval, returns a domain.ApprovalRequester ready to
// attach to ctx. Prefers TenantApprovalRequester (identity-aware — correct
// for a shared, multi-tenant Executor) over a plain domain.ApprovalRequester
// (correct for a single-tenant or test/fake Executor).
func resolveApprovalRequester(hosted *HostedAgent, conv domain.ConversationRef) (domain.ApprovalRequester, bool) {
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		return nil, false
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return nil, false
	}

	if tenant, ok := executor.(TenantApprovalRequester); ok {
		return approvalRequesterFunc(func(ctx context.Context, conv domain.ConversationRef, prompt string, timeoutSeconds int) (bool, error) {
			connectionRef, _ := domain.ConnectionRefFromContext(ctx, identity.ID)
			return tenant.RequestApproval(ctx, identity, connectionRef, conv, prompt, timeoutSeconds)
		}), true
	}
	if requester, ok := executor.(domain.ApprovalRequester); ok {
		return requester, true
	}
	return nil, false
}

// TenantApprovalRequester is the identity-aware counterpart to
// domain.ApprovalRequester — the shape a multi-tenant-safe implementation
// actually needs (see domain.InteractiveMessenger's own doc comment for
// why). HandleTurn (host.go) prefers this over the plain
// domain.ApprovalRequester when a conversation's Executor implements it,
// binding the resolved Identity+connectionRef before exposing it to
// domain.HydrateTool's approval gate as an ordinary ApprovalRequester —
// hydrate.go never needs to know multi-tenancy exists at all.
type TenantApprovalRequester interface {
	RequestApproval(ctx context.Context, identity *domain.Identity, connectionRef string, conv domain.ConversationRef, prompt string, timeoutSeconds int) (approved bool, err error)
}

// defaultApprovalTimeout is used when RequestApproval is called with
// timeoutSeconds <= 0 — domain.ToolDefinition.ApprovalTimeoutSeconds's own
// doc comment documents zero as "the runtime host's own default"; this is
// that default, matching kael-platform's own (10 minutes).
const defaultApprovalTimeout = 10 * time.Minute

// MessengerApprovalRequester is the framework's real, shipped
// TenantApprovalRequester — provider-agnostic orchestration (correlation
// ID, a wait channel per pending request, timeout) built on top of
// whatever domain.InteractiveMessenger a provider supplies. This is safe
// to ship as a concrete implementation, unlike Executor or Memory, because
// none of this logic is provider-specific — only Messenger's two methods
// are (see domain.NativeLoop for the same reasoning: ship the generic
// orchestration, leave only the true provider edge to each implementation).
type MessengerApprovalRequester struct {
	Messenger domain.InteractiveMessenger

	mu      sync.Mutex
	waiters map[string]chan bool
	seq     atomic.Uint64
}

func NewMessengerApprovalRequester(messenger domain.InteractiveMessenger) *MessengerApprovalRequester {
	return &MessengerApprovalRequester{Messenger: messenger, waiters: make(map[string]chan bool)}
}

var _ TenantApprovalRequester = (*MessengerApprovalRequester)(nil)

// RequestApproval posts an approval prompt via Messenger, then blocks until
// either a matching Resolve call arrives or timeoutSeconds (or
// defaultApprovalTimeout, if <= 0) elapses — never "keep waiting"
// indefinitely, same stance kael-platform's own approval gate takes.
func (r *MessengerApprovalRequester) RequestApproval(ctx context.Context, identity *domain.Identity, connectionRef string, conv domain.ConversationRef, prompt string, timeoutSeconds int) (bool, error) {
	token := fmt.Sprintf("approval-%d", r.seq.Add(1))
	ch := make(chan bool, 1)

	r.mu.Lock()
	r.waiters[token] = ch
	r.mu.Unlock()

	cleanup := func() {
		r.mu.Lock()
		delete(r.waiters, token)
		r.mu.Unlock()
	}

	messageID, err := r.Messenger.PostApprovalPrompt(ctx, identity, connectionRef, conv, prompt, token)
	if err != nil {
		cleanup()
		return false, fmt.Errorf("request approval: post prompt: %w", err)
	}

	timeout := defaultApprovalTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case approved := <-ch:
		outcome := "❌ Rejected — nothing was submitted."
		if approved {
			outcome = "✅ Approved — proceeding…"
		}
		_ = r.Messenger.UpdateMessage(context.Background(), identity, connectionRef, conv, messageID, outcome)
		return approved, nil
	case <-waitCtx.Done():
		cleanup()
		_ = r.Messenger.UpdateMessage(context.Background(), identity, connectionRef, conv, messageID, "⏱ Timed out — nothing happened.")
		return false, waitCtx.Err()
	}
}

// Resolve is called by whatever transport receives the provider's
// button-click callback (see examples/executors/slack's
// HandleInteractivity for the reference), once it has determined token and
// approved. Returns false if no pending wait matches token — already
// resolved, already timed out, or simply unknown — the caller decides what
// that means for its own response (typically: ack the provider's webhook
// with 200 regardless, and just log the miss).
func (r *MessengerApprovalRequester) Resolve(token string, approved bool) bool {
	r.mu.Lock()
	ch, ok := r.waiters[token]
	if ok {
		delete(r.waiters, token)
	}
	r.mu.Unlock()

	if !ok {
		return false
	}
	ch <- approved
	return true
}
