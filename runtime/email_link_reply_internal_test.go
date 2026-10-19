package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// sayer records what the agent says to the person.
type sayer struct {
	mu   sync.Mutex
	said []string
}

func (s *sayer) Execute(_ context.Context, _ *domain.Identity, _ string, _ string, input map[string]any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.said = append(s.said, input["text"].(string))
	return "ok", nil
}

func (s *sayer) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.said) == 0 {
		return ""
	}
	return s.said[len(s.said)-1]
}

// linkWorld is a host with the email-link flow wired to fakes.
type linkWorld struct {
	h        *Host
	hosted   *HostedAgent
	says     *sayer
	state    string // what the database holds for the chat
	setTo    []string
	started  []string // "person identity chat email"
	startErr error
}

func newLinkWorld(t *testing.T) *linkWorld {
	t.Helper()
	w := &linkWorld{h: NewHost(), says: &sayer{}}
	integration := &domain.Integration{ID: "int_slack", Service: "slack"}
	identity := &domain.Identity{ID: "id_slack", IntegrationID: integration.ID}
	execs := domain.NewExecutorRegistry()
	execs.Register("slack", w.says)
	w.hosted = &HostedAgent{
		Agent: &domain.Agent{ID: "a1"},
		Deps: AgentDeps{
			Executors:        execs,
			IntegrationsByID: map[string]*domain.Integration{integration.ID: integration},
			IdentitiesByID:   map[string]*domain.Identity{identity.ID: identity},
		},
	}
	w.h.SetEmailLinkFlow(
		func(context.Context, string) (bool, error) { return false, nil },
		func(context.Context, string, string) (string, error) { return w.state, nil },
		func(_ context.Context, _, _, state string) error {
			w.state = state
			w.setTo = append(w.setTo, state)
			return nil
		},
		func(_ context.Context, person, identity, chat, email string) error {
			if w.startErr != nil {
				return w.startErr
			}
			w.started = append(w.started, person+" "+identity+" "+chat+" "+email)
			return nil
		},
	)
	return w
}

func (w *linkWorld) say(text string) bool {
	return w.h.handleEmailLinkReply(context.Background(), w.hosted, domain.InboundMessage{
		Text:         text,
		Conversation: domain.ConversationRef{Provider: "slack", IdentityID: "id_slack", ChatID: "C1", UserID: "p1"},
	})
}

const chatKey = "id_slack:C1"

func TestEmailLinkReply_AChatThatWasNeverAskedIsLeftAlone(t *testing.T) {
	w := newLinkWorld(t)
	if w.say("hello, what's my email policy?") {
		t.Error("an ordinary message goes on to the agent")
	}
	if len(w.says.said) != 0 || len(w.started) != 0 {
		t.Error("nothing is said or started")
	}
	// Nor without a known person, or without the flow wired at all.
	w.h.pendingEmailLinks.Store(chatKey, time.Now())
	if (&Host{}).handleEmailLinkReply(context.Background(), w.hosted, domain.InboundMessage{Text: "a@b.co", Conversation: domain.ConversationRef{IdentityID: "id_slack", ChatID: "C1", UserID: "p1"}}) {
		t.Error("no flow, no handling")
	}
	if w.h.handleEmailLinkReply(context.Background(), w.hosted, domain.InboundMessage{Text: "a@b.co", Conversation: domain.ConversationRef{IdentityID: "id_slack", ChatID: "C1"}}) {
		t.Error("no known person, no handling")
	}
}

func TestEmailLinkReply_AnAddressStartsTheVerification(t *testing.T) {
	w := newLinkWorld(t)
	w.h.pendingEmailLinks.Store(chatKey, time.Now())
	if !w.say("sure, it's ada@example.com thanks") {
		t.Fatal("the reply is taken")
	}
	if len(w.started) != 1 || w.started[0] != "p1 id_slack C1 ada@example.com" {
		t.Errorf("verification starts for the person who asked: %v", w.started)
	}
	if w.state != "sent" || !strings.Contains(w.says.last(), "Check your inbox") {
		t.Errorf("%q %q", w.state, w.says.last())
	}
	if _, still := w.h.pendingEmailLinks.Load(chatKey); still {
		t.Error("no longer waiting")
	}
	if w.say("another message") {
		t.Error("and the next message is a message again")
	}
}

func TestEmailLinkReply_SkipAndNotAnAddress(t *testing.T) {
	w := newLinkWorld(t)
	w.h.pendingEmailLinks.Store(chatKey, time.Now())
	if !w.say("SKIP") || w.state != "skipped" || len(w.started) != 0 {
		t.Errorf("skip cancels: %q %v", w.state, w.started)
	}
	if w.say("hello") {
		t.Error("and it is over")
	}

	w.h.pendingEmailLinks.Store(chatKey, time.Now().Add(-10*time.Minute))
	if !w.say("what?") || !strings.Contains(w.says.last(), "doesn't look like a valid email") {
		t.Errorf("asked again: %q", w.says.last())
	}
	if v, ok := w.h.pendingEmailLinks.Load(chatKey); !ok || time.Since(v.(time.Time)) > time.Minute {
		t.Error("asking again gives them a fresh while to answer")
	}
	if len(w.started) != 0 {
		t.Error("nothing started")
	}
}

func TestEmailLinkReply_AFailureToSendIsSaidAndNotMarkedSent(t *testing.T) {
	w := newLinkWorld(t)
	w.startErr = errors.New("mail provider down")
	w.h.pendingEmailLinks.Store(chatKey, time.Now())
	if !w.say("ada@example.com") || !strings.Contains(w.says.last(), "Something went wrong") {
		t.Errorf("%q", w.says.last())
	}
	for _, s := range w.setTo {
		if s == "sent" {
			t.Error("not marked sent when it was not")
		}
	}
}

// The bug this fixes: the wait was saved but never read back, so a restart between "send me your
// email" and the reply made the reply an ordinary message.
func TestEmailLinkReply_TheWaitSurvivesARestart(t *testing.T) {
	w := newLinkWorld(t)
	w.state = "awaiting" // saved before the restart; nothing in memory
	if !w.say("ada@example.com") {
		t.Fatal("the reply is still recognised as the email")
	}
	if len(w.started) != 1 {
		t.Errorf("%v", w.started)
	}
}

func TestEmailLinkReply_AWaitThatLapsedIsForgotten(t *testing.T) {
	w := newLinkWorld(t)
	// In memory: asked long ago.
	w.h.pendingEmailLinks.Store(chatKey, time.Now().Add(-domain.EmailLinkAwaitingFor-time.Minute))
	if w.say("ada@example.com") {
		t.Error("an old wait does not capture an ordinary message")
	}
	if _, still := w.h.pendingEmailLinks.Load(chatKey); still {
		t.Error("and is forgotten")
	}
	// Just inside the window it still counts.
	w.h.pendingEmailLinks.Store(chatKey, time.Now().Add(-domain.EmailLinkAwaitingFor+time.Minute))
	if !w.say("ada@example.com") {
		t.Error("a reply within the time is the email")
	}
	// A value that is not a time (nothing writes one) never counts.
	w.h.pendingEmailLinks.Store(chatKey, true)
	if w.say("ada@example.com") {
		t.Error("only a time counts")
	}
}

func TestEmailLinkAwaitingState(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	for name, c := range map[string]struct {
		ch   domain.MessengerChannel
		want string
	}{
		"fresh wait":            {domain.MessengerChannel{EmailLinkState: "awaiting", EmailLinkStateAt: ago(time.Minute)}, "awaiting"},
		"just inside":           {domain.MessengerChannel{EmailLinkState: "awaiting", EmailLinkStateAt: ago(domain.EmailLinkAwaitingFor)}, "awaiting"},
		"lapsed":                {domain.MessengerChannel{EmailLinkState: "awaiting", EmailLinkStateAt: ago(domain.EmailLinkAwaitingFor + time.Second)}, ""},
		"from before times":     {domain.MessengerChannel{EmailLinkState: "awaiting"}, ""},
		"sent never lapses":     {domain.MessengerChannel{EmailLinkState: "sent", EmailLinkStateAt: ago(1000 * time.Hour)}, "sent"},
		"verified never lapses": {domain.MessengerChannel{EmailLinkState: "verified"}, "verified"},
		"nothing":               {domain.MessengerChannel{}, ""},
	} {
		if got := c.ch.EffectiveEmailLinkState(now); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
