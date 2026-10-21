package runtime

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// maxCandidates bounds how many connections are tried one by one for a delivery that names none.
const maxCandidates = 50

// ConnectionLoader returns every connection of a source that has a secret.
type ConnectionLoader func(ctx context.Context, source string) ([]*domain.Connection, error)

// CachedResolver is a domain.ConnectionResolver that loads a source's connections once in a while instead of for
// every delivery. If a refresh fails it keeps serving what it had.
type CachedResolver struct {
	load ConnectionLoader
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	loaded  map[string]time.Time
	byKey   map[string]map[string][]*domain.Connection // source -> route key -> connections
	all     map[string][]*domain.Connection            // source -> every connection
	refresh map[string]*sync.Mutex                     // one refresh at a time per source
}

func NewCachedResolver(load ConnectionLoader, ttl time.Duration) *CachedResolver {
	return &CachedResolver{
		load: load, ttl: ttl, now: time.Now,
		loaded: map[string]time.Time{}, byKey: map[string]map[string][]*domain.Connection{},
		all: map[string][]*domain.Connection{}, refresh: map[string]*sync.Mutex{},
	}
}

var _ domain.ConnectionResolver = (*CachedResolver)(nil)

// Resolve returns the connections with this route key, or every connection of the source for an empty key.
func (r *CachedResolver) Resolve(ctx context.Context, source, routeKey string) ([]*domain.Connection, error) {
	r.ensure(ctx, source)
	r.mu.Lock()
	defer r.mu.Unlock()
	if routeKey == "" {
		all := r.all[source]
		if len(all) > maxCandidates {
			all = all[:maxCandidates]
		}
		return append([]*domain.Connection(nil), all...), nil
	}
	return append([]*domain.Connection(nil), r.byKey[source][routeKey]...), nil
}

// Invalidate makes the next delivery of the source reload its connections (call it when an identity changes).
func (r *CachedResolver) Invalidate(source string) {
	r.mu.Lock()
	delete(r.loaded, source)
	r.mu.Unlock()
}

func (r *CachedResolver) fresh(source string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.loaded[source]
	return ok && r.now().Sub(at) < r.ttl
}

// ensure reloads the source's connections if they are stale. Concurrent deliveries share one reload.
func (r *CachedResolver) ensure(ctx context.Context, source string) {
	if r.fresh(source) {
		return
	}
	r.mu.Lock()
	gate := r.refresh[source]
	if gate == nil {
		gate = &sync.Mutex{}
		r.refresh[source] = gate
	}
	r.mu.Unlock()

	gate.Lock()
	defer gate.Unlock()
	if r.fresh(source) { // another delivery reloaded while this one waited
		return
	}
	conns, err := r.load(ctx, source)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		log.Printf("runtime: connections for %q could not be reloaded (keeping what there was): %v", source, err)
		if _, had := r.loaded[source]; !had {
			r.loaded[source] = time.Time{} // nothing yet: try again on the next delivery
			delete(r.loaded, source)
		}
		return
	}
	idx := map[string][]*domain.Connection{}
	for _, c := range conns {
		idx[c.RouteKey] = append(idx[c.RouteKey], c)
	}
	r.byKey[source], r.all[source], r.loaded[source] = idx, conns, r.now()
}
