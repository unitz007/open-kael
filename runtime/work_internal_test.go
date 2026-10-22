package runtime

import (
	"sync"
	"testing"
)

// Overlapping work keeps an agent working until the last piece is done, and watchers hear only of the change from idle
// to working and back, not of every piece. Ending a piece twice changes nothing.
func TestWorkBoard_OverlappingWorkAndOnlyRealChanges(t *testing.T) {
	h := NewHost()
	ch, stop := h.WatchWork([]string{"a"})
	defer stop()
	drain := func() int {
		n := 0
		for {
			select {
			case <-ch:
				n++
			default:
				return n
			}
		}
	}
	end1 := h.beginWork("a")
	if drain() != 1 {
		t.Fatal("told it started")
	}
	end2 := h.beginWork("a")
	if drain() != 0 {
		t.Fatal("a second piece is not news")
	}
	end1()
	end1() // twice: no effect
	if got := h.Working([]string{"a"}); len(got) != 1 || drain() != 0 {
		t.Fatalf("still working on the other piece: %v", got)
	}
	end2()
	if got := h.Working([]string{"a"}); len(got) != 0 || drain() != 1 {
		t.Fatalf("idle now, and told: %v", got)
	}
	stop()
	h.beginWork("a")()
	if drain() != 0 {
		t.Fatal("a watcher that stopped is not told")
	}
}

func TestWorkBoard_IsSafeUnderManyTurns(t *testing.T) {
	h := NewHost()
	ch, stop := h.WatchWork([]string{"a", "b"})
	defer stop()
	go func() {
		for range ch {
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "a"
			if i%2 == 0 {
				id = "b"
			}
			done := h.beginWork(id)
			h.Working([]string{"a", "b"})
			done()
		}(i)
	}
	wg.Wait()
	if got := h.Working([]string{"a", "b"}); len(got) != 0 {
		t.Fatalf("all done: %v", got)
	}
}
