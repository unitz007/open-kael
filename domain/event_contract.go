package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The event contract.
//
// An event is something that happened on a connection: a pull request was opened, an invoice
// was paid. It only tells; what anyone does about it is a separate, gated step. Every event,
// whatever integration it comes from, must be complete when it is registered:
//
//   - a Name that starts with its integration's service, is unique across the platform and
//     stays the same across deploys (people's rules refer to it);
//   - a Description in plain words, with none of the platform's own vocabulary;
//   - an Arrival: how it reaches us. "pushed" means the service posts it to us and a Handler
//     decodes the delivery;
//   - a Subject: who it is about. Every event says how that person is found, so a heads-up can
//     only ever go to the right person. "payload" names the property that identifies them (a
//     login, a customer) and what kind of account that is; "nobody" is for feeds that are about
//     no one;
//   - a Payload schema: an object with typed, described properties and real required ones. What
//     a Handler returns is held to it, so whatever reacts to the event is told structured facts;
//   - Options: the settings a person may give (which repository, how long before). May be empty;
//   - a RepeatKey: the required property that identifies one occurrence, so the same one is
//     never handled twice;
//   - a Sample payload that satisfies the schema, for previews and tests;
//   - optionally a Summary: a one-line message with {{blanks}} that are required properties of the payload.
//
// Events enter through IntegrationEventRegistry.Register, which holds each to this contract:
// one that breaks it is left out and reported, and the rest load. How an event is *used* is the
// other half: see IntegrationEvent.CheckPayload.

// EventArrival says how an event reaches us.
type EventArrival string

const (
	// ArrivalPushed: the service posts the event to us, verified with the connection's secret.
	ArrivalPushed EventArrival = "pushed"
	// ArrivalInternal: the platform raised it itself.
	ArrivalInternal EventArrival = "internal"
)

// SubjectKind says how the person an event is about is found.
type SubjectKind string

const (
	// SubjectPayload: a property of the payload identifies them (a login, a customer id).
	SubjectPayload SubjectKind = "payload"
	// SubjectNobody: the event is about no one (a feed). It can only go to a destination named for it.
	SubjectNobody SubjectKind = "nobody"
)

// EventSubject says who an event is about.
type EventSubject struct {
	Kind SubjectKind `json:"kind"`
	// Field is the required string property of the payload that identifies the person (SubjectPayload).
	Field string `json:"field,omitempty"`
	// Account says what kind of account that identifier belongs to, such as "github_login" or
	// "stripe_customer" (SubjectPayload). It is how the identifier is matched to a person.
	Account string `json:"account,omitempty"`
}

const (
	// MaxEventDescriptionLen keeps an event's description to a short sentence.
	MaxEventDescriptionLen = 120
	// MaxEventPayloadProperties keeps what an agent is told about an event small.
	MaxEventPayloadProperties = 20
)

var (
	eventNameRe    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
	eventAccountRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// platformWords are the platform's own vocabulary, which a person never sees.
	platformWords = regexp.MustCompile(`(?i)\b(events?|triggers?|webhooks?|payloads?|json|api|tools?|skills?|callbacks?|endpoints?)\b`)
)

// EventProblem is one way an event breaks the contract.
type EventProblem struct {
	Event   string // the event's name
	Message string
}

func (p EventProblem) Error() string { return fmt.Sprintf("event %q: %s", p.Event, p.Message) }

// ValidateEvent reports every way e breaks the contract (nil when it keeps it). service is the
// integration's service key, which the event's name must start with.
func ValidateEvent(service string, e *IntegrationEvent) error {
	if e == nil {
		return errors.New("event is nil")
	}
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, EventProblem{Event: e.Name, Message: fmt.Sprintf(format, a...)})
	}

	switch {
	case !eventNameRe.MatchString(e.Name):
		bad("the name must be lowercase words separated by dots, such as %s.thing.happened", service)
	case !strings.HasPrefix(e.Name, service+"."):
		bad("the name must start with its integration, %q", service+".")
	}
	switch d := strings.TrimSpace(e.Description); {
	case d == "":
		bad("has no description")
	case len(d) > MaxEventDescriptionLen:
		bad("description is %d characters; the most allowed is %d", len(d), MaxEventDescriptionLen)
	case platformWords.MatchString(d):
		bad("description uses a platform word (%q); say what happened in plain words", platformWords.FindString(d))
	}

	switch e.Arrival {
	case ArrivalPushed:
		if e.Handler == nil {
			bad("is pushed to us but has no handler to decode a delivery")
		}
	case ArrivalInternal:
	default:
		bad("must say how it arrives (pushed or internal), not %q", e.Arrival)
	}

	props := e.Payload.Properties
	if e.Payload.Type != SchemaTypeObject {
		bad("payload must be an object, not %q", e.Payload.Type)
	}
	if len(props) == 0 {
		bad("payload describes nothing")
	}
	if len(props) > MaxEventPayloadProperties {
		bad("payload has %d properties; the most allowed is %d", len(props), MaxEventPayloadProperties)
	}
	badProps := func(what string, s Schema) {
		names := make([]string, 0, len(s.Properties))
		for n := range s.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := s.Properties[n]
			if p.Type == "" {
				bad("%s %q has no type", what, n)
			}
			if strings.TrimSpace(p.Description) == "" {
				bad("%s %q has no description", what, n)
			}
			if p.Type == SchemaTypeArray && (p.Items == nil || p.Items.Type == "") {
				bad("%s %q is a list without a type for its items", what, n)
			}
		}
		for _, r := range s.Required {
			if _, ok := s.Properties[r]; !ok {
				bad("%s requires %q, which it doesn't describe", what, r)
			}
		}
	}
	badProps("payload property", e.Payload)
	if e.Options.Type != "" && e.Options.Type != SchemaTypeObject {
		bad("options must be an object, not %q", e.Options.Type)
	}
	badProps("option", e.Options)

	required := func(name string) bool {
		for _, r := range e.Payload.Required {
			if r == name {
				return true
			}
		}
		return false
	}
	switch e.Subject.Kind {
	case SubjectPayload:
		f, ok := props[e.Subject.Field]
		switch {
		case e.Subject.Field == "":
			bad("says it is about someone in the payload but not which property")
		case !ok:
			bad("the property that says who it is about, %q, is not in the payload", e.Subject.Field)
		case f.Type != SchemaTypeString:
			bad("the property that says who it is about, %q, must be text", e.Subject.Field)
		case !required(e.Subject.Field):
			bad("the property that says who it is about, %q, must be required", e.Subject.Field)
		}
		if !eventAccountRe.MatchString(e.Subject.Account) {
			bad("must say what kind of account the person is identified by, such as github_login")
		}
	case SubjectNobody:
		if e.Subject.Field != "" || e.Subject.Account != "" {
			bad("is about nobody, so it has no property or account for who")
		}
	default:
		bad("must say who it is about, not %q", e.Subject.Kind)
	}

	switch k, ok := props[e.RepeatKey]; {
	case e.RepeatKey == "":
		bad("has no repeat key, so the same occurrence could be handled twice")
	case !ok:
		bad("the repeat key %q is not in the payload", e.RepeatKey)
	case k.Type != SchemaTypeString && k.Type != SchemaTypeInteger:
		bad("the repeat key %q must be text or a whole number", e.RepeatKey)
	case !required(e.RepeatKey):
		bad("the repeat key %q must be required", e.RepeatKey)
	}

	for _, msg := range summaryProblems(e) {
		bad("%s", msg)
	}

	if e.Sample == nil {
		bad("has no sample payload")
	} else if len(props) > 0 {
		if _, err := e.Payload.CheckInput(e.Sample); err != nil {
			bad("the sample payload doesn't satisfy its own schema (%v)", err)
		}
	}
	return errors.Join(errs...)
}

// ValidateEvents checks one integration's events together: each must keep the contract and no
// two may share a name.
func ValidateEvents(service string, events []*IntegrationEvent) error {
	_, err := ValidEvents(service, events)
	return err
}

// ValidEvents returns the events that keep the contract, and an error describing every one that
// does not (and any name used twice, of which only the first is kept).
func ValidEvents(service string, events []*IntegrationEvent) ([]*IntegrationEvent, error) {
	var errs []error
	var keep []*IntegrationEvent
	seen := map[string]bool{}
	for _, e := range events {
		if err := ValidateEvent(service, e); err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[e.Name] {
			errs = append(errs, EventProblem{Event: e.Name, Message: "is used by more than one event"})
			continue
		}
		seen[e.Name] = true
		keep = append(keep, e)
	}
	return keep, errors.Join(errs...)
}

// CheckPayload makes sure what a handler produced is what the event's schema says (a JSON object
// with the required properties, each of the right kind), and returns it decoded. Whatever
// reacts to an event only ever sees a payload that passed.
func (e *IntegrationEvent) CheckPayload(payload string) (map[string]any, error) {
	if e.Payload.Type != SchemaTypeObject {
		return nil, fmt.Errorf("event %q: it describes no payload, so a delivery can't be checked", e.Name)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(payload), &in); err != nil {
		return nil, fmt.Errorf("event %q: the payload is not a JSON object: %w", e.Name, err)
	}
	out, err := e.Payload.CheckInput(in)
	if err != nil {
		return nil, fmt.Errorf("event %q: %w", e.Name, err)
	}
	return out, nil
}
