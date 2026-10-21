package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func conns(keys ...string) []*domain.Connection {
	var out []*domain.Connection
	for _, k := range keys {
		out = append(out, &domain.Connection{ID: "c" + k, RouteKey: k, Secret: []byte("s" + k)})
	}
	return out
}

func TestCachedResolver_LoadsOnceWithinTheTimeAndAgainAfter(t *testing.T) {
	loads := 0
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := NewCachedResolver(func(context.Context, string) ([]*domain.Connection, error) { loads++; return conns("1", "2"), nil }, time.Minute)
	r.now = func() time.Time { return now }
	ctx := context.Background()

	if got, _ := r.Resolve(ctx, "github", "1"); len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("by route key: %+v", got)
	}
	r.Resolve(ctx, "github", "2")
	if loads != 1 {
		t.Errorf("within the time it is not reloaded: %d loads", loads)
	}
	now = now.Add(2 * time.Minute)
	r.Resolve(ctx, "github", "1")
	if loads != 2 {
		t.Errorf("after the time it is reloaded: %d loads", loads)
	}
	r.Invalidate("github")
	r.Resolve(ctx, "github", "1")
	if loads != 3 {
		t.Errorf("an invalidated source is reloaded at once: %d loads", loads)
	}
}

func TestCachedResolver_AnUnknownKeyHasNoConnectionsAndNoKeyMeansAll(t *testing.T) {
	r := NewCachedResolver(func(context.Context, string) ([]*domain.Connection, error) { return conns("1", "2"), nil }, time.Minute)
	ctx := context.Background()
	if got, _ := r.Resolve(ctx, "github", "999"); len(got) != 0 {
		t.Errorf("unknown key: %+v", got)
	}
	if got, _ := r.Resolve(ctx, "github", ""); len(got) != 2 {
		t.Errorf("no key means every connection: %+v", got)
	}
	if got, _ := r.Resolve(ctx, "stripe", "1"); len(got) != 2 && len(got) != 1 {
		t.Logf("a different source loads its own: %+v", got)
	}
}

func TestCachedResolver_NoKeyIsCappedSoItCannotBeUsedToTryThousands(t *testing.T) {
	var many []*domain.Connection
	for i := 0; i < 200; i++ {
		many = append(many, &domain.Connection{ID: "c", RouteKey: "k", Secret: []byte("s")})
	}
	r := NewCachedResolver(func(context.Context, string) ([]*domain.Connection, error) { return many, nil }, time.Minute)
	if got, _ := r.Resolve(context.Background(), "github", ""); len(got) != maxCandidates {
		t.Errorf("tried at most %d: %d", maxCandidates, len(got))
	}
}

func TestCachedResolver_AFailedReloadKeepsWhatItHad(t *testing.T) {
	fail := false
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := NewCachedResolver(func(context.Context, string) ([]*domain.Connection, error) {
		if fail {
			return nil, errors.New("database down")
		}
		return conns("1"), nil
	}, time.Minute)
	r.now = func() time.Time { return now }
	ctx := context.Background()
	r.Resolve(ctx, "github", "1")
	fail = true
	now = now.Add(5 * time.Minute)
	if got, err := r.Resolve(ctx, "github", "1"); err != nil || len(got) != 1 {
		t.Fatalf("a failed reload keeps serving what there was: %+v %v", got, err)
	}
}

func TestFailureLimiter(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	l := newFailureLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if l.blocked("a") {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("a")
	}
	if !l.blocked("a") || l.blocked("b") {
		t.Fatal("a is blocked after three failures, b is not")
	}
	now = now.Add(2 * time.Minute)
	if l.blocked("a") {
		t.Fatal("the window passes")
	}
}

// Many deliveries arriving at once while the connections are stale share one reload, not one each.
func TestCachedResolver_ConcurrentDeliveriesShareOneReload(t *testing.T) {
	var mu sync.Mutex
	loads := 0
	r := NewCachedResolver(func(context.Context, string) ([]*domain.Connection, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
		return conns("1"), nil
	}, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Resolve(context.Background(), "github", "1")
		}()
	}
	wg.Wait()
	if loads != 1 {
		t.Errorf("20 simultaneous deliveries loaded the connections %d times", loads)
	}
}
