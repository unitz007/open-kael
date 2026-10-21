package runtime

import (
	"context"
	"io"
	"log"
	"net/http"

	"github.com/unitz007/open-kael/domain"
)

// RegisterEventSources wires an HTTP handler for each EventSource onto mux so external services can POST events:
// the handler verifies the signature, calls the event's Handler to get the canonical event name, facts and the
// account it is about, checks the facts against the event's payload schema, and tells the registered event
// dispatcher (see SetEventDispatcher). A delivery whose facts don't match the schema is dropped and logged.
func (h *Host) RegisterEventSources(mux *http.ServeMux, sources *domain.EventSourceRegistry) {
	for _, entry := range sources.All() {
		entry := entry
		mux.HandleFunc(entry.Source.Path(), func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			if !entry.Source.Verify(body, r.Header) {
				http.Error(w, "signature verification failed", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
			for _, ev := range entry.Events {
				payload, actorExternalID, ok, err := ev.Handler(body, r.Header)
				if err != nil {
					log.Printf("runtime: event handler %q: %v", ev.Name, err)
					return
				}
				if ok {
					// What reacts to an event only ever sees a payload that matches what the event promised.
					if _, err := ev.CheckPayload(payload); err != nil {
						log.Printf("runtime: dropping %q: %v", ev.Name, err)
						return
					}
					h.dispatchEvent(context.Background(), ev.Name, payload, entry.IdentityID, actorExternalID)
					return
				}
			}
		})
		log.Printf("runtime: event source registered at %s", entry.Source.Path())
	}
}

// dispatchEvent tells the registered event dispatcher about an event, in its own goroutine.
func (h *Host) dispatchEvent(ctx context.Context, eventName, payload, sourceIdentityID, actorExternalID string) {
	if h.eventDispatcher == nil {
		return
	}
	go func() {
		defer recoverFromPanic("", "dispatching event "+eventName)
		h.eventDispatcher(ctx, eventName, payload, sourceIdentityID, actorExternalID)
	}()
}
