package runtime

import (
	"sync"
	"time"
)

// failureLimiter stops one client from hammering an endpoint with deliveries that fail the signature check: after max
// failures within window it refuses everything from that client until the window has passed. Successful deliveries
// are never counted.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	seen   map[string]*failures
}

type failures struct {
	count int
	since time.Time
}

func newFailureLimiter(max int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: max, window: window, now: time.Now, seen: map[string]*failures{}}
}

// blocked reports whether the client has failed too often lately.
func (l *failureLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.seen[key]
	if !ok {
		return false
	}
	if l.now().Sub(f.since) > l.window {
		delete(l.seen, key)
		return false
	}
	return f.count >= l.max
}

// fail counts one failure for the client.
func (l *failureLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.seen) > 10000 { // forget old clients so this never grows without end
		for k, f := range l.seen {
			if now.Sub(f.since) > l.window {
				delete(l.seen, k)
			}
		}
	}
	f, ok := l.seen[key]
	if !ok || now.Sub(f.since) > l.window {
		l.seen[key] = &failures{count: 1, since: now}
		return
	}
	f.count++
}
