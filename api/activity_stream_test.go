package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/api"
	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// sseClient reads the events of a live stream as they arrive.
type sseClient struct {
	resp   *http.Response
	events chan sseEvent
}

type sseEvent struct {
	name string
	data string
}

func openStream(t *testing.T, base, token, path string) *sseClient {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	c := &sseClient{resp: resp, events: make(chan sseEvent, 64)}
	if resp.StatusCode != http.StatusOK {
		return c
	}
	go func() {
		defer close(c.events)
		sc := bufio.NewScanner(resp.Body)
		var name, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if name != "" {
					c.events <- sseEvent{name, data}
				}
				name, data = "", ""
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return c
}

func (c *sseClient) next(t *testing.T, within time.Duration) (domain.ActivitySummary, bool) {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			return domain.ActivitySummary{}, false
		}
		if ev.name != "summary" {
			t.Fatalf("unexpected event %q", ev.name)
		}
		var sum domain.ActivitySummary
		if err := json.Unmarshal([]byte(ev.data), &sum); err != nil {
			t.Fatalf("bad payload: %v %s", err, ev.data)
		}
		return sum, true
	case <-time.After(within):
		return domain.ActivitySummary{}, false
	}
}

// streamRig is a server with activity switched on, over a database of its own, with two creators
// (stm_a with two agents, stm_b with one) signed in.
func streamRig(t *testing.T, refresh time.Duration, opts ...api.Option) (*postgres.Store, *httptest.Server) {
	t.Helper()
	ctx := context.Background()
	store, _ := pgtest.Store(t)
	srv := httptest.NewServer(api.NewServer(store, append([]api.Option{api.WithActivity(store), api.WithActivityRefresh(refresh)}, opts...)...))
	t.Cleanup(srv.Close)

	users := []string{"stm_a", "stm_b"}
	for _, u := range users {
		if err := store.SaveAccount(ctx, &domain.Account{ID: u, Email: u + "@example.com"}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveSession(ctx, &domain.Session{Token: "tok_" + u, AccountID: u, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []*domain.Agent{
		{ID: "stm_ag_a", Name: "A's", CreatedBy: "stm_a", MaxIterations: 5},
		{ID: "stm_ag_a2", Name: "A's second", CreatedBy: "stm_a", MaxIterations: 5},
		{ID: "stm_ag_b", Name: "B's", CreatedBy: "stm_b", MaxIterations: 5},
	} {
		if err := store.SaveAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	return store, srv
}

// The screen is told the moment something happens: one update at once, another within a moment of
// anything being recorded for its agents, and none for agents that are not its own.
func TestActivityStream_TellsTheScreenAtOnceAndWhenSomethingHappens(t *testing.T) {
	store, srv := streamRig(t, time.Minute) // a minute: the pushes below can only come from changes
	ctx := context.Background()
	c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream?days=7")
	if c.resp.StatusCode != 200 || !strings.HasPrefix(c.resp.Header.Get("Content-Type"), "text/event-stream") || c.resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("a stream that proxies won't hold back: %d %v", c.resp.StatusCode, c.resp.Header)
	}
	first, ok := c.next(t, 3*time.Second)
	if !ok || first.RecentMessages != 0 || len(first.Daily) != 7 {
		t.Fatalf("an update straight away: %+v %v", first, ok)
	}

	store.MessageHandled(ctx, "stm_ag_a")
	got, ok := c.next(t, 3*time.Second)
	if !ok || got.RecentMessages != 1 {
		t.Fatalf("a message is pushed within moments: %+v %v", got, ok)
	}
	store.ActivityEvent(ctx, "stm_ag_a2", domain.ActivityFailed, "unavailable")
	if got, ok = c.next(t, 3*time.Second); !ok || got.FailedToday != 1 || len(got.Events) == 0 || got.Events[0].Kind != domain.ActivityFailed {
		t.Fatalf("an event from the creator's other agent is pushed too: %+v %v", got, ok)
	}
	store.PersonActive(ctx, "stm_ag_a", "someone")
	if got, ok = c.next(t, 3*time.Second); !ok || got.ActiveToday != 1 {
		t.Fatalf("a person newly active: %+v %v", got, ok)
	}

	// Someone else's agent: nothing is pushed, and nothing of it is visible.
	store.MessageHandled(ctx, "stm_ag_b")
	store.ActivityEvent(ctx, "stm_ag_b", domain.ActivityFailed, "problem")
	if got, ok = c.next(t, 800*time.Millisecond); ok {
		t.Errorf("another creator's agent must not wake this screen: %+v", got)
	}
}

// Changes that arrive together are one update, not a flood.
func TestActivityStream_ManyChangesTogetherAreOneUpdate(t *testing.T) {
	store, srv := streamRig(t, time.Minute)
	ctx := context.Background()
	c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream?agent_id=stm_ag_a")
	if _, ok := c.next(t, 3*time.Second); !ok {
		t.Fatal("no first update")
	}
	for i := 0; i < 20; i++ {
		store.MessageHandled(ctx, "stm_ag_a")
	}
	got, ok := c.next(t, 3*time.Second)
	if !ok || got.RecentMessages == 0 {
		t.Fatalf("an update: %+v %v", got, ok)
	}
	updates := 1
	for {
		if _, more := c.next(t, 600*time.Millisecond); !more {
			break
		}
		updates++
	}
	if updates > 3 {
		t.Errorf("20 changes at once must not become 20 updates: %d", updates)
	}
}

// With nothing happening it still asks again, so numbers that fade with time stay true.
func TestActivityStream_AsksAgainEvenWhenNothingHappens(t *testing.T) {
	_, srv := streamRig(t, 300*time.Millisecond)
	c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream")
	for i := 0; i < 3; i++ {
		if _, ok := c.next(t, 2*time.Second); !ok {
			t.Fatalf("update %d never came", i)
		}
	}
}

func TestActivityStream_OnlyForTheCreatorsOwnAgentsAndNeverToAnAnonymousCaller(t *testing.T) {
	_, srv := streamRig(t, time.Minute)
	for path, want := range map[string]int{
		"/activity/stream?agent_id=stm_ag_b": http.StatusNotFound,
		"/activity/stream?agent_id=nope":     http.StatusNotFound,
		"/activity/stream?days=99":           http.StatusBadRequest,
	} {
		if c := openStream(t, srv.URL, "tok_stm_a", path); c.resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, c.resp.StatusCode, want)
		}
	}
	resp, err := http.Get(srv.URL + "/activity/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no sign-in: %d", resp.StatusCode)
	}
}

// A closed screen stops being watched, and a person can only hold a few open.
func TestActivityStream_ClosedScreensAreLetGoAndAPersonCannotHoldManyOpen(t *testing.T) {
	store, srv := streamRig(t, time.Minute)
	var open []*sseClient
	for i := 0; i < 4; i++ {
		c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream")
		if c.resp.StatusCode != 200 {
			t.Fatalf("stream %d: %d", i, c.resp.StatusCode)
		}
		open = append(open, c)
	}
	if c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream"); c.resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a fifth open view: %d", c.resp.StatusCode)
	}
	if c := openStream(t, srv.URL, "tok_stm_b", "/activity/stream"); c.resp.StatusCode != 200 {
		t.Errorf("someone else is not held back by A's views: %d", c.resp.StatusCode)
	}
	var wg sync.WaitGroup
	for _, c := range open {
		wg.Add(1)
		go func(c *sseClient) { defer wg.Done(); c.resp.Body.Close() }(c)
	}
	wg.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c := openStream(t, srv.URL, "tok_stm_a", "/activity/stream"); c.resp.StatusCode == 200 {
			_ = store
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("after closing, the slots are given back")
}
