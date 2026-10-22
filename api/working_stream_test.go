package api_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/api"
)

// fakeWork is a host whose agents' working state the test sets.
type fakeWork struct {
	mu   sync.Mutex
	busy map[string]bool
	subs []chan struct{}
}

func (f *fakeWork) Working(ids []string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, id := range ids {
		if f.busy[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeWork) WatchWork(ids []string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fakeWork) set(id string, busy bool) {
	f.mu.Lock()
	if f.busy == nil {
		f.busy = map[string]bool{}
	}
	f.busy[id] = busy
	subs := append([]chan struct{}(nil), f.subs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func nextWorking(t *testing.T, c *sseClient) []string {
	t.Helper()
	select {
	case ev, ok := <-c.events:
		if !ok {
			t.Fatal("the stream ended")
		}
		if ev.name != "working" {
			t.Fatalf("unexpected event %q", ev.name)
		}
		var body struct {
			Working []string `json:"working"`
		}
		if err := json.Unmarshal([]byte(ev.data), &body); err != nil || body.Working == nil {
			t.Fatalf("bad payload %q: %v", ev.data, err)
		}
		return body.Working
	case <-time.After(3 * time.Second):
		t.Fatal("nothing arrived")
	}
	return nil
}

// A creator's screen hears at once which of their agents are working, and again when one starts or stops; never about
// someone else's agent.
func TestWorkingStream_TellsWhoIsWorkingAndWhenThatChanges(t *testing.T) {
	work := &fakeWork{}
	work.set("stm_ag_b", true) // someone else's agent, busy
	_, srv, _ := streamRig(t, time.Minute, api.WithWorkWatcher(work))

	c := openStream(t, srv.URL, "tok_stm_a", "/activity/working/stream")
	if c.resp.StatusCode != http.StatusOK {
		t.Fatalf("%d", c.resp.StatusCode)
	}
	if got := nextWorking(t, c); len(got) != 0 {
		t.Fatalf("nobody of theirs is working: %v", got)
	}
	work.set("stm_ag_a", true)
	if got := nextWorking(t, c); len(got) != 1 || got[0] != "stm_ag_a" {
		t.Fatalf("their agent started: %v", got)
	}
	work.set("stm_ag_a", false)
	if got := nextWorking(t, c); len(got) != 0 {
		t.Fatalf("and stopped: %v", got)
	}

	one := openStream(t, srv.URL, "tok_stm_a", "/activity/working/stream?agent_id=stm_ag_a2")
	if one.resp.StatusCode != http.StatusOK {
		t.Fatalf("one of their own: %d", one.resp.StatusCode)
	}
	if theirs := openStream(t, srv.URL, "tok_stm_a", "/activity/working/stream?agent_id=stm_ag_b"); theirs.resp.StatusCode != http.StatusNotFound {
		t.Fatalf("someone else's agent is not theirs to watch: %d", theirs.resp.StatusCode)
	}
}

func TestWorkingStream_NotAvailableWithoutAWatcher(t *testing.T) {
	_, srv, _ := streamRig(t, time.Minute)
	c := openStream(t, srv.URL, "tok_stm_a", "/activity/working/stream")
	if c.resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("%d", c.resp.StatusCode)
	}
}
