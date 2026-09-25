package runtime_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

func TestPeer_DispatchRoundTrip(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()

	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = runtime.Dial(ctx, addr, "secret-token", "worker-1", func(_ context.Context, taskID, text string, _ []domain.ActionSpec, _ runtime.SideCaller) (string, error) {
			return "did: " + text, nil
		})
	}()

	// Wait for the peer to actually register before dispatching — Dial's
	// hello handshake is async relative to this goroutine starting.
	deadline := time.Now().Add(2 * time.Second)
	for !hub.Connected("worker-1") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for worker-1 to connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	result, err := hub.Dispatch(context.Background(), "worker-1", "run the tests", nil, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != "did: run the tests" {
		t.Fatalf("unexpected result: %q", result)
	}
}

func TestPeer_DispatchToUnknownPeerFails(t *testing.T) {
	hub := runtime.NewPeerHub()
	_, err := hub.Dispatch(context.Background(), "never-connected", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected an error dispatching to a peer that never connected")
	}
}

func TestPeer_DialRejectsWrongToken(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()
	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	err := runtime.Dial(context.Background(), addr, "wrong-token", "worker-1", func(context.Context, string, string, []domain.ActionSpec, runtime.SideCaller) (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("expected Dial to fail with the wrong token")
	}
}

func TestPeer_HandlerErrorPropagatesToDispatch(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()
	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = runtime.Dial(ctx, addr, "secret-token", "worker-2", func(context.Context, string, string, []domain.ActionSpec, runtime.SideCaller) (string, error) {
			return "", fmt.Errorf("boom")
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !hub.Connected("worker-2") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for worker-2 to connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := hub.Dispatch(context.Background(), "worker-2", "do it", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected the handler's error to propagate, got %v", err)
	}
}

// TestPeer_SideCallRoundTripsBeforeResult proves a peer can call back to
// the hub mid-task (e.g. Claude Code posting a status update via an MCP
// tool) and get an answer before its own task result is sent — the
// mechanism the MCP-tool relay in WizerAgents' cmd/claude-code-pair
// depends on.
func TestPeer_SideCallRoundTripsBeforeResult(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()
	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = runtime.Dial(ctx, addr, "secret-token", "worker-3", func(ctx context.Context, _ string, text string, _ []domain.ActionSpec, sideCall runtime.SideCaller) (string, error) {
			out, err := sideCall(ctx, "post_update", map[string]any{"text": "halfway through " + text})
			if err != nil {
				return "", fmt.Errorf("side call: %w", err)
			}
			if posted, _ := out["posted"].(bool); !posted {
				return "", fmt.Errorf("expected the side call's answer to report posted=true, got %+v", out)
			}
			return "finished: " + text, nil
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !hub.Connected("worker-3") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for worker-3 to connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var sideCallSeen struct {
		action string
		input  map[string]any
	}
	onSideCall := func(_ context.Context, action string, input map[string]any) (map[string]any, error) {
		sideCallSeen.action = action
		sideCallSeen.input = input
		return map[string]any{"posted": true}, nil
	}

	result, err := hub.Dispatch(context.Background(), "worker-3", "the mr review", nil, onSideCall)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != "finished: the mr review" {
		t.Fatalf("unexpected result: %q", result)
	}
	if sideCallSeen.action != "post_update" {
		t.Fatalf("expected onSideCall to see action %q, got %q", "post_update", sideCallSeen.action)
	}
	wantInput := map[string]any{"text": "halfway through the mr review"}
	if !reflect.DeepEqual(sideCallSeen.input, wantInput) {
		t.Fatalf("unexpected side call input: %+v, want %+v", sideCallSeen.input, wantInput)
	}
}

// TestPeer_DispatchActionsReachTaskHandler proves the actions Dispatch is
// given actually cross the wire to the peer's TaskHandler — the mechanism
// a Loop implementation backed by an external agentic process (not a
// per-turn LLM) needs to expose those actions as its own callable tools
// for the one task it's running.
func TestPeer_DispatchActionsReachTaskHandler(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()
	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seenNames []string
	go func() {
		_ = runtime.Dial(ctx, addr, "secret-token", "worker-5", func(_ context.Context, _ string, text string, actions []domain.ActionSpec, _ runtime.SideCaller) (string, error) {
			for _, a := range actions {
				seenNames = append(seenNames, a.Name)
			}
			return "did: " + text, nil
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !hub.Connected("worker-5") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for worker-5 to connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	actions := []domain.ActionSpec{
		{Name: "create_issue", Description: "Opens a GitHub issue"},
		{Name: "list_issues", Description: "Lists GitHub issues"},
	}
	result, err := hub.Dispatch(context.Background(), "worker-5", "manage the issues", actions, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != "did: manage the issues" {
		t.Fatalf("unexpected result: %q", result)
	}
	if len(seenNames) != 2 || seenNames[0] != "create_issue" || seenNames[1] != "list_issues" {
		t.Fatalf("expected the handler to see both action names in order, got %v", seenNames)
	}
}

// TestPeer_DispatchWithoutSideCallHandlerStillCompletes proves passing nil
// for onSideCall (every pre-existing caller before this change) is still a
// fully valid Dispatch — no regression for a task that never issues a side
// call, which is the common case today.
func TestPeer_DispatchWithoutSideCallHandlerStillCompletes(t *testing.T) {
	hub := runtime.NewPeerHub()
	srv := httptest.NewServer(hub.Handler("secret-token"))
	defer srv.Close()
	addr := "http" + strings.TrimPrefix(srv.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = runtime.Dial(ctx, addr, "secret-token", "worker-4", func(_ context.Context, _ string, text string, _ []domain.ActionSpec, _ runtime.SideCaller) (string, error) {
			return "did: " + text, nil
		})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !hub.Connected("worker-4") {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for worker-4 to connect")
		}
		time.Sleep(10 * time.Millisecond)
	}

	result, err := hub.Dispatch(context.Background(), "worker-4", "run the tests", nil, nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result != "did: run the tests" {
		t.Fatalf("unexpected result: %q", result)
	}
}
