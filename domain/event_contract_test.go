package domain_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func goodEvent() *domain.IntegrationEvent {
	return &domain.IntegrationEvent{
		Name:        "acme.order.placed",
		Description: "An order was placed",
		Arrival:     domain.ArrivalPushed,
		Subject:     domain.EventSubject{Kind: domain.SubjectPayload, Field: "buyer", Account: "acme_customer"},
		Payload: domain.Schema{Type: domain.SchemaTypeObject, Required: []string{"delivery", "buyer"}, Properties: map[string]domain.Schema{
			"delivery": {Type: domain.SchemaTypeString, Description: "The delivery's own id."},
			"buyer":    {Type: domain.SchemaTypeString, Description: "Who placed it."},
			"total":    {Type: domain.SchemaTypeNumber, Description: "What it came to."},
		}},
		Options:   domain.Schema{Type: domain.SchemaTypeObject, Properties: map[string]domain.Schema{"shop": {Type: domain.SchemaTypeString, Description: "Only this shop."}}},
		RepeatKey: "delivery",
		Sample:    map[string]any{"delivery": "d1", "buyer": "ann", "total": 12.5},
		Handler:   func([]byte, http.Header) (string, string, bool, error) { return "", "", false, nil },
	}
}

func TestValidateEvent_AGoodEventPasses(t *testing.T) {
	if err := domain.ValidateEvent("acme", goodEvent()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateEvent_EachWayToBreakTheContractIsReported(t *testing.T) {
	for name, tc := range map[string]struct {
		break_ func(*domain.IntegrationEvent)
		want   string
	}{
		"name not under its integration": {func(e *domain.IntegrationEvent) { e.Name = "other.order.placed" }, "must start with its integration"},
		"name with capitals":             {func(e *domain.IntegrationEvent) { e.Name = "acme.Order" }, "lowercase words"},
		"no description":                 {func(e *domain.IntegrationEvent) { e.Description = " " }, "no description"},
		"too long a description":         {func(e *domain.IntegrationEvent) { e.Description = strings.Repeat("a", 121) }, "the most allowed"},
		"platform words":                 {func(e *domain.IntegrationEvent) { e.Description = "A webhook event fired" }, "platform word"},
		"no way of arriving":             {func(e *domain.IntegrationEvent) { e.Arrival = "" }, "how it arrives"},
		"pushed with no handler":         {func(e *domain.IntegrationEvent) { e.Handler = nil }, "no handler"},
		"no subject":                     {func(e *domain.IntegrationEvent) { e.Subject = domain.EventSubject{} }, "who it is about"},
		"subject property missing":       {func(e *domain.IntegrationEvent) { e.Subject.Field = "nobody_here" }, "not in the payload"},
		"subject property not required":  {func(e *domain.IntegrationEvent) { e.Payload.Required = []string{"delivery"} }, "must be required"},
		"subject not text":               {func(e *domain.IntegrationEvent) { e.Subject.Field = "total" }, "must be text"},
		"subject without an account":     {func(e *domain.IntegrationEvent) { e.Subject.Account = "" }, "what kind of account"},
		"nobody with a property": {func(e *domain.IntegrationEvent) {
			e.Subject = domain.EventSubject{Kind: domain.SubjectNobody, Field: "buyer"}
		}, "about nobody"},
		"no payload":       {func(e *domain.IntegrationEvent) { e.Payload = domain.Schema{} }, "payload must be an object"},
		"untyped property": {func(e *domain.IntegrationEvent) { e.Payload.Properties["total"] = domain.Schema{Description: "x"} }, `has no type`},
		"undescribed property": {func(e *domain.IntegrationEvent) {
			e.Payload.Properties["total"] = domain.Schema{Type: domain.SchemaTypeNumber}
		}, `has no description`},
		"list without item type": {func(e *domain.IntegrationEvent) {
			e.Payload.Properties["tags"] = domain.Schema{Type: domain.SchemaTypeArray, Description: "x"}
		}, "without a type for its items"},
		"required but undescribed":      {func(e *domain.IntegrationEvent) { e.Payload.Required = append(e.Payload.Required, "ghost") }, `requires "ghost"`},
		"no repeat key":                 {func(e *domain.IntegrationEvent) { e.RepeatKey = "" }, "no repeat key"},
		"repeat key not in payload":     {func(e *domain.IntegrationEvent) { e.RepeatKey = "ghost" }, "not in the payload"},
		"repeat key not required":       {func(e *domain.IntegrationEvent) { e.Payload.Required = []string{"buyer"} }, "must be required"},
		"repeat key not text or number": {func(e *domain.IntegrationEvent) { e.RepeatKey = "total" }, "text or a whole number"},
		"undescribed option": {func(e *domain.IntegrationEvent) {
			e.Options.Properties["shop"] = domain.Schema{Type: domain.SchemaTypeString}
		}, "option"},
		"no sample":                     {func(e *domain.IntegrationEvent) { e.Sample = nil }, "no sample"},
		"sample that breaks the schema": {func(e *domain.IntegrationEvent) { e.Sample = map[string]any{"delivery": "d1"} }, "doesn't satisfy"},
	} {
		e := goodEvent()
		tc.break_(e)
		err := domain.ValidateEvent("acme", e)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a problem mentioning %q, got %v", name, tc.want, err)
		}
	}
}

func TestIntegrationEventRegistry_LeavesOutWhatBreaksTheContractAndLoadsTheRest(t *testing.T) {
	r := domain.NewIntegrationEventRegistry()
	broken := goodEvent()
	broken.Name = "acme.order.broken"
	broken.Sample = nil
	twin := goodEvent() // same name as the first
	err := r.Register("acme", []*domain.IntegrationEvent{goodEvent(), broken, twin})
	if err == nil || !strings.Contains(err.Error(), "no sample") || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("both problems are reported: %v", err)
	}
	if got := r.EventsFor("acme"); len(got) != 1 || got[0].Name != "acme.order.placed" {
		t.Fatalf("only the good, first one loads: %v", got)
	}
	if _, ok := r.Event("acme.order.placed"); !ok {
		t.Error("it can be found by name")
	}
}

func TestIntegrationEventRegistry_ANameAnotherIntegrationHoldsIsRefused(t *testing.T) {
	r := domain.NewIntegrationEventRegistry()
	held := goodEvent()
	held.Name = "acme.shop.order.placed" // valid under "acme" and, being nested, under "acme.shop" too
	if err := r.Register("acme", []*domain.IntegrationEvent{held}); err != nil {
		t.Fatal(err)
	}
	clash := goodEvent()
	clash.Name = "acme.shop.order.placed"
	err := r.Register("acme.shop", []*domain.IntegrationEvent{clash})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("a name held by another integration is refused: %v", err)
	}
	if len(r.EventsFor("acme.shop")) != 0 {
		t.Error("and not loaded")
	}
}

func TestCheckPayload_HoldsADeliveryToTheSchema(t *testing.T) {
	e := goodEvent()
	if got, err := e.CheckPayload(`{"delivery":"d1","buyer":"ann","total":"5"}`); err != nil || got["total"] != 5.0 {
		t.Errorf("a harmless difference is converted: %v %v", got, err)
	}
	for name, p := range map[string]string{"missing required": `{"delivery":"d1"}`, "not json": `nope`, "not an object": `[1]`, "wrong kind": `{"delivery":"d1","buyer":"a","total":"lots"}`} {
		if _, err := e.CheckPayload(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	e.Payload = domain.Schema{}
	if _, err := e.CheckPayload(`{}`); err == nil {
		t.Error("an event that describes no payload can't check one")
	}
}
