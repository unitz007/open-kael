package postgres

import (
	"testing"
	"time"
)

func signalled(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(30 * time.Millisecond):
		return false
	}
}

// A watcher hears about its own agents only, never blocks the one recording, and stops hearing once
// it lets go. Two watchers of the same agent both hear.
func TestActivityHub_WatchersHearOnlyTheirAgentsAndNeverBlockTheSender(t *testing.T) {
	s := &Store{}
	a, stopA := s.WatchActivity([]string{"ag1", "ag2"})
	b, stopB := s.WatchActivity([]string{"ag1"})
	other, stopOther := s.WatchActivity([]string{"ag9"})
	none, stopNone := s.WatchActivity(nil)
	defer stopOther()
	defer stopNone()

	s.notifyActivity("ag1")
	if !signalled(a) || !signalled(b) {
		t.Error("both watchers of ag1 hear it")
	}
	if signalled(other) || signalled(none) {
		t.Error("a watcher of other agents, or of none, must not be woken")
	}

	// Many changes while nobody reads: the sender is never blocked, and one signal is waiting.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.notifyActivity("ag2")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("recording must never wait for a watcher")
	}
	if !signalled(a) {
		t.Error("the change is waiting for the watcher")
	}
	if signalled(a) {
		t.Error("a thousand changes are one waiting signal, not a thousand")
	}
	if signalled(b) {
		t.Error("b does not watch ag2")
	}

	stopA()
	stopA() // stopping twice is harmless
	stopB()
	s.notifyActivity("ag1")
	if signalled(a) || signalled(b) {
		t.Error("a watcher that let go hears nothing")
	}
	if n := s.watchers(); n != 2 {
		t.Errorf("the two that remain: %d", n)
	}
}
