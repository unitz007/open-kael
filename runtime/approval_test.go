package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// fakeInteractiveMessenger records posted prompts and lets a test read
// back the token RequestApproval generated, without any real HTTP.
type fakeInteractiveMessenger struct {
	posted chan string // tokens, one per PostApprovalPrompt call
	failIt bool
}

func (f *fakeInteractiveMessenger) PostApprovalPrompt(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, _ string, token string) (string, error) {
	if f.failIt {
		return "", context.DeadlineExceeded
	}
	f.posted <- token
	return "msg-1", nil
}

func (f *fakeInteractiveMessenger) UpdateMessage(_ context.Context, _ *domain.Identity, _ string, _ domain.ConversationRef, _ string, _ string) error {
	return nil
}

func TestMessengerApprovalRequester_ApprovedViaResolve(t *testing.T) {
	messenger := &fakeInteractiveMessenger{posted: make(chan string, 1)}
	requester := runtime.NewMessengerApprovalRequester(messenger)
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1"}

	type outcome struct {
		approved bool
		err      error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		approved, err := requester.RequestApproval(context.Background(), nil, "", conv, "do it?", 5)
		resultCh <- outcome{approved, err}
	}()

	token := <-messenger.posted
	if !requester.Resolve(token, true) {
		t.Fatal("expected Resolve to find the pending wait")
	}

	got := <-resultCh
	if got.err != nil || !got.approved {
		t.Fatalf("expected approved=true, nil error, got approved=%v err=%v", got.approved, got.err)
	}
}

func TestMessengerApprovalRequester_TimesOut(t *testing.T) {
	messenger := &fakeInteractiveMessenger{posted: make(chan string, 1)}
	requester := runtime.NewMessengerApprovalRequester(messenger)
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1"}

	approved, err := requester.RequestApproval(context.Background(), nil, "", conv, "do it?", 1)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if approved {
		t.Fatal("expected approved=false on timeout")
	}
}

func TestMessengerApprovalRequester_ResolveAfterTimeoutIsIgnored(t *testing.T) {
	messenger := &fakeInteractiveMessenger{posted: make(chan string, 1)}
	requester := runtime.NewMessengerApprovalRequester(messenger)
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1"}

	go requester.RequestApproval(context.Background(), nil, "", conv, "do it?", 1)
	token := <-messenger.posted
	time.Sleep(1500 * time.Millisecond) // let it time out first

	if requester.Resolve(token, true) {
		t.Fatal("expected Resolve to report no pending wait after timeout cleanup")
	}
}

func TestMessengerApprovalRequester_UnknownTokenResolveReturnsFalse(t *testing.T) {
	requester := runtime.NewMessengerApprovalRequester(&fakeInteractiveMessenger{posted: make(chan string, 1)})
	if requester.Resolve("never-requested", true) {
		t.Fatal("expected Resolve to report no pending wait for an unknown token")
	}
}
