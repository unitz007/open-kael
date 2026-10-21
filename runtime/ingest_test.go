package runtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// fakeAdapter is a push adapter: the route key is the X-Route header, a delivery is signed when X-Sig equals the
// connection's secret, and the body "ping" is not an event.
type fakeAdapter struct{ decodeErr error }

func (fakeAdapter) Source() string        { return "fake" }
func (fakeAdapter) LegacyPaths() []string { return []string{"/webhooks/fake"} }
func (fakeAdapter) RouteKey(r *http.Request, _ []byte) (string, bool) {
	k := r.Header.Get("X-Route")
	return k, k != ""
}
func (fakeAdapter) Verify(secret []byte, r *http.Request, _ []byte) bool {
	return string(secret) == r.Header.Get("X-Sig")
}
func (a fakeAdapter) Decode(r *http.Request, body []byte) (*domain.Occurrence, bool, error) {
	if a.decodeErr != nil {
		return nil, false, a.decodeErr
	}
	if string(body) == "ping" {
		return nil, false, nil
	}
	id := r.Header.Get("X-Delivery")
	return &domain.Occurrence{DeliveryID: id, Event: "fake.thing.happened", Facts: map[string]any{"title": string(body)},
		SubjectKind: "fake_account", SubjectValue: "octo", RepeatKey: id}, true, nil
}

type memInbox struct {
	mu   sync.Mutex
	rows []*domain.Occurrence
	err  error
}

func (m *memInbox) Insert(_ context.Context, o *domain.Occurrence) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, false, m.err
	}
	for _, r := range m.rows {
		if r.Source == o.Source && r.DeliveryID == o.DeliveryID {
			return 0, false, nil
		}
	}
	m.rows = append(m.rows, o)
	return int64(len(m.rows)), true, nil
}
func (m *memInbox) Counts(context.Context) (map[string]int, error) {
	return map[string]int{"new": len(m.rows)}, nil
}
func (m *memInbox) stored() []*domain.Occurrence {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*domain.Occurrence(nil), m.rows...)
}

type staticResolver map[string][]*domain.Connection // route key -> connections; "" lists them all

func (s staticResolver) Resolve(_ context.Context, _, key string) ([]*domain.Connection, error) {
	return s[key], nil
}

type ingestRig struct {
	mux     *http.ServeMux
	inbox   *memInbox
	metrics *runtime.Metrics
	fwd     []*domain.Occurrence
	mu      sync.Mutex
}

func newIngestRig(a fakeAdapter, opts runtime.IngestOptions) *ingestRig {
	c1 := &domain.Connection{ID: "c1", RouteKey: "1", Secret: []byte("s1")}
	c2 := &domain.Connection{ID: "c2", RouteKey: "2", Secret: []byte("s2")}
	res := staticResolver{"1": {c1}, "2": {c2}, "": {c1, c2}}
	r := &ingestRig{mux: http.NewServeMux(), inbox: &memInbox{}, metrics: runtime.NewMetrics()}
	opts.Metrics = r.metrics
	opts.Forward = func(_ context.Context, o *domain.Occurrence) {
		r.mu.Lock()
		r.fwd = append(r.fwd, o)
		r.mu.Unlock()
	}
	runtime.MountIngest(r.mux, []domain.PushAdapter{a}, res, r.inbox, opts)
	return r
}

func (r *ingestRig) post(path, body string, h map[string]string, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	if remote != "" {
		req.RemoteAddr = remote + ":1234"
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func (r *ingestRig) forwarded() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.fwd)
}

func TestIngest_AValidDeliveryIsStoredAcknowledgedAndForwardedOnce(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	h := map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "d1"}
	if rec := r.post("/events/fake", "hello", h, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	rows := r.inbox.stored()
	if len(rows) != 1 || rows[0].ConnectionID != "c1" || rows[0].Source != "fake" || rows[0].Facts["title"] != "hello" || rows[0].SubjectValue != "octo" || rows[0].ReceivedAt.IsZero() {
		t.Fatalf("stored with the connection, the source and the facts: %+v", rows)
	}
	if r.forwarded() != 1 {
		t.Fatalf("forwarded: %d", r.forwarded())
	}
	// The same delivery again (a redelivery): acknowledged, not stored or forwarded twice.
	if rec := r.post("/events/fake", "hello", h, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if len(r.inbox.stored()) != 1 || r.forwarded() != 1 {
		t.Errorf("a redelivery is a no-op: stored %d, forwarded %d", len(r.inbox.stored()), r.forwarded())
	}
	snap := r.metrics.Snapshot()["fake"]
	if snap["received"] != 2 || snap["stored"] != 1 || snap["duplicate"] != 1 || snap["forwarded"] != 1 {
		t.Errorf("counters: %v", snap)
	}
}

func TestIngest_ALegacyAddressStillWorks(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	if rec := r.post("/webhooks/fake", "x", map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "d1"}, ""); rec.Code != http.StatusOK || len(r.inbox.stored()) != 1 {
		t.Fatalf("%d %d", rec.Code, len(r.inbox.stored()))
	}
}

func TestIngest_AnUnsignedOrUnknownDeliveryIsRefusedTheSameWayAndKeepsNothing(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	wrong := r.post("/events/fake", "x", map[string]string{"X-Route": "1", "X-Sig": "nope", "X-Delivery": "d1"}, "")
	unknown := r.post("/events/fake", "x", map[string]string{"X-Route": "999", "X-Sig": "s1", "X-Delivery": "d2"}, "")
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized || wrong.Body.String() != unknown.Body.String() {
		t.Fatalf("both are the same 401, so the endpoint cannot be used to find out which apps exist: %d %q / %d %q", wrong.Code, wrong.Body, unknown.Code, unknown.Body)
	}
	if len(r.inbox.stored()) != 0 || r.forwarded() != 0 {
		t.Error("a refused delivery is neither stored nor forwarded")
	}
	snap := r.metrics.Snapshot()["fake"]
	if snap["rejected"] != 1 || snap["unknown"] != 1 {
		t.Errorf("counted apart: %v", snap)
	}
}

func TestIngest_ADeliveryIsCheckedAgainstTheConnectionItNamesOnly(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	// Names app 2 but is signed with app 1's secret: refused, not accepted as app 1's.
	if rec := r.post("/events/fake", "x", map[string]string{"X-Route": "2", "X-Sig": "s1", "X-Delivery": "d1"}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d", rec.Code)
	}
	if rec := r.post("/events/fake", "x", map[string]string{"X-Route": "2", "X-Sig": "s2", "X-Delivery": "d2"}, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if rows := r.inbox.stored(); len(rows) != 1 || rows[0].ConnectionID != "c2" {
		t.Fatalf("stored under the connection that signed it: %+v", rows)
	}
}

func TestIngest_ADeliveryThatNamesNoConnectionIsTriedAgainstEachOne(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	if rec := r.post("/events/fake", "x", map[string]string{"X-Sig": "s2", "X-Delivery": "d1"}, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if rows := r.inbox.stored(); len(rows) != 1 || rows[0].ConnectionID != "c2" {
		t.Fatalf("found the connection whose secret signed it: %+v", rows)
	}
}

func TestIngest_APingAndAnUnreadableDeliveryAreAcknowledgedAndKeepNothing(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	h := map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "d1"}
	if rec := r.post("/events/fake", "ping", h, ""); rec.Code != http.StatusOK || len(r.inbox.stored()) != 0 {
		t.Fatalf("a ping: %d", rec.Code)
	}
	bad := newIngestRig(fakeAdapter{decodeErr: errors.New("garbled")}, runtime.IngestOptions{})
	if rec := bad.post("/events/fake", "x", h, ""); rec.Code != http.StatusOK || len(bad.inbox.stored()) != 0 || bad.metrics.Snapshot()["fake"]["error"] != 1 {
		t.Fatalf("a delivery we cannot read is acknowledged (the sender cannot fix it) and counted: %d", rec.Code)
	}
}

func TestIngest_WhatCouldNotBeStoredIsNotAcknowledgedSoItIsSentAgain(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	r.inbox.err = errors.New("database down")
	rec := r.post("/events/fake", "x", map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "d1"}, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a delivery we did not keep must not be acknowledged: %d", rec.Code)
	}
	if r.forwarded() != 0 {
		t.Error("and it is not acted on")
	}
}

func TestIngest_ADeliveryThatIsTooLargeIsRefused(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{MaxBytes: 10})
	rec := r.post("/events/fake", strings.Repeat("a", 100), map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "d1"}, "")
	if rec.Code != http.StatusRequestEntityTooLarge || len(r.inbox.stored()) != 0 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestIngest_AClientThatKeepsFailingIsThrottledAndOthersAreNot(t *testing.T) {
	r := newIngestRig(fakeAdapter{}, runtime.IngestOptions{})
	bad := map[string]string{"X-Route": "1", "X-Sig": "nope", "X-Delivery": "d"}
	for i := 0; i < 30; i++ {
		if rec := r.post("/events/fake", "x", bad, "6.6.6.6"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, rec.Code)
		}
	}
	good := map[string]string{"X-Route": "1", "X-Sig": "s1", "X-Delivery": "g1"}
	if rec := r.post("/events/fake", "x", good, "6.6.6.6"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after too many failures even a good delivery from that client waits: %d", rec.Code)
	}
	if rec := r.post("/events/fake", "x", good, "7.7.7.7"); rec.Code != http.StatusOK {
		t.Fatalf("another client is not affected: %d", rec.Code)
	}
}
