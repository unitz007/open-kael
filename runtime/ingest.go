package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/unitz007/open-kael/domain"
)

const (
	// DefaultMaxDeliveryBytes is the largest delivery that is read; a bigger one is refused.
	DefaultMaxDeliveryBytes = 5 << 20
	// failuresBeforeThrottle and failureWindow decide when a client that keeps failing the signature check is refused outright.
	failuresBeforeThrottle = 30
	failureWindow          = time.Minute
)

// IngestOptions configures MountIngest.
type IngestOptions struct {
	// MaxBytes is the largest delivery read (DefaultMaxDeliveryBytes when zero).
	MaxBytes int64
	// Forward, when set, is told of each stored occurrence. While the rest of the pipeline is being built this is how
	// the existing, in-memory path keeps acting on events; it is called after the occurrence is stored.
	Forward func(ctx context.Context, o *domain.Occurrence)
	Metrics *Metrics
}

// MountIngest mounts the thin front of the events pipeline: for each adapter, POST /events/{source} (and the
// addresses it says must keep working). A delivery is routed to its connection, checked against that connection's
// secret, decoded, stored in the inbox and acknowledged. Nothing else happens here: no fan-out, no model call.
//
// Every refusal looks the same to the client (401) whether the connection is unknown or the signature is wrong, so
// the endpoint cannot be used to find out which apps exist, and a client that fails the check too often is throttled.
func MountIngest(mux *http.ServeMux, adapters []domain.PushAdapter, res domain.ConnectionResolver, inbox domain.InboxStore, opts IngestOptions) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxDeliveryBytes
	}
	limiter := newFailureLimiter(failuresBeforeThrottle, failureWindow)
	for _, a := range adapters {
		a := a
		handler := func(w http.ResponseWriter, r *http.Request) {
			ingest(w, r, a, res, inbox, opts, limiter)
		}
		paths := append([]string{"/events/" + a.Source()}, a.LegacyPaths()...)
		for _, p := range paths {
			mux.HandleFunc("POST "+p, handler)
			log.Printf("runtime: ingest for %q at %s", a.Source(), p)
		}
	}
}

func clientOf(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func ingest(w http.ResponseWriter, r *http.Request, a domain.PushAdapter, res domain.ConnectionResolver, inbox domain.InboxStore, opts IngestOptions, limiter *failureLimiter) {
	src := a.Source()
	m := opts.Metrics
	m.Inc(src, "received")
	client := clientOf(r)
	if limiter.blocked(client) {
		m.Inc(src, "throttled")
		http.Error(w, "too many failed deliveries", http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, opts.MaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			m.Inc(src, "rejected")
			http.Error(w, "delivery too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	refuse := func(counter string) {
		m.Inc(src, counter)
		limiter.fail(client)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
	}

	key, _ := a.RouteKey(r, body)
	candidates, err := res.Resolve(r.Context(), src, key)
	if err != nil {
		m.Inc(src, "error")
		log.Printf("runtime: ingest %s: resolve connection: %v", src, err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	var conn *domain.Connection
	for _, c := range candidates {
		if a.Verify(c.Secret, r, body) {
			conn = c
			break
		}
	}
	if conn == nil {
		if len(candidates) == 0 {
			refuse("unknown")
		} else {
			refuse("rejected")
		}
		return
	}

	occ, ok, err := a.Decode(r, body)
	if err != nil {
		m.Inc(src, "error")
		log.Printf("runtime: ingest %s: decode: %v", src, err)
		w.WriteHeader(http.StatusOK) // the sender cannot fix a delivery we cannot read; retrying would not help
		return
	}
	if !ok {
		m.Inc(src, "ignored")
		w.WriteHeader(http.StatusOK)
		return
	}
	occ.Source, occ.ConnectionID, occ.ReceivedAt = src, conn.ID, time.Now()

	if _, inserted, err := inbox.Insert(r.Context(), occ); err != nil {
		// Not stored: say so, so the sender sends it again. Acknowledging what we did not keep is how events get lost.
		m.Inc(src, "error")
		log.Printf("runtime: ingest %s: store: %v", src, err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	} else if !inserted {
		m.Inc(src, "duplicate")
		w.WriteHeader(http.StatusOK)
		return
	}
	m.Inc(src, "stored")
	w.WriteHeader(http.StatusOK)
	if opts.Forward != nil {
		opts.Forward(context.WithoutCancel(r.Context()), occ)
		m.Inc(src, "forwarded")
	}
}

// DispatchOccurrence tells the registered event dispatcher about an occurrence, in its own goroutine. It is how the
// existing in-memory path acts on what ingest has stored.
func (h *Host) DispatchOccurrence(ctx context.Context, o *domain.Occurrence) {
	payload, err := json.Marshal(o.Facts)
	if err != nil {
		log.Printf("runtime: dispatch %q: %v", o.Event, err)
		return
	}
	h.dispatchEvent(ctx, o.Event, string(payload), o.ConnectionID, o.SubjectValue)
}
