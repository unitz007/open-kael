package runtime_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// zoneRig is an agent whose one ability depends on the person's clock (a calendar booking): the model
// calls it, and then finishes. The calendar records what it was asked to do.
type zoneRig struct {
	host     *runtime.Host
	hosted   *runtime.HostedAgent
	box      *outbox
	calendar *recordingCalendar
	llm      *zoneLLM
	mu       sync.Mutex
	zone     string
	links    int
}

// zoneLLM calls the ability on the first look at each request and finishes on the next, and keeps
// the request it was looking at.
type zoneLLM struct {
	mu   sync.Mutex
	asks []string // the person's message each time a turn began
	last string
}

func (z *zoneLLM) Call(_ context.Context, msgs []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	user := ""
	var sawTool bool
	for _, m := range msgs {
		if m.Role == domain.RoleUser {
			user = m.Content
		}
		if m.Role == domain.RoleTool {
			sawTool = true
		}
	}
	if !sawTool {
		z.asks = append(z.asks, user)
		return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "t", Name: "calendar", Arguments: map[string]any{}}}}, nil
	}
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": ""}}}}, nil
}

func (z *zoneLLM) turns() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.asks...)
}

type recordingCalendar struct {
	mu    sync.Mutex
	calls int
}

func (c *recordingCalendar) Execute(context.Context, *domain.Identity, string, string, map[string]any) (any, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return map[string]any{"message": "booked"}, nil
}
func (c *recordingCalendar) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

func newZoneRig(t *testing.T, needsLocalTime bool, zone string, withLink bool) *zoneRig {
	t.Helper()
	r := &zoneRig{box: &outbox{}, calendar: &recordingCalendar{}, zone: zone}
	integ := &domain.Integration{ID: "int-cal", Service: "cal"}
	tool := &domain.ToolDefinition{ID: "book", Name: "book_slot", FunctionName: "book_slot", Description: "Book a slot.", Action: "b", IntegrationID: integ.ID, NeedsLocalTime: needsLocalTime}
	skill := &domain.Skill{ID: "s1", Name: "calendar", Description: "Your calendar", AgentID: "a1", Tools: []domain.ToolBinding{{ToolID: "book"}}, Integrations: []string{integ.ID}}
	// The model books once; after the booking (or the question) it finishes.
	r.llm = &zoneLLM{}
	agent := &domain.Agent{ID: "a1", Name: "Desk", LLMs: []domain.LLM{r.llm}, MaxIterations: 6, Skills: []*domain.Skill{skill}}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", r.box)
	executors.Register("cal", r.calendar)
	r.host = runtime.NewHost()
	r.hosted = &runtime.HostedAgent{Agent: agent, Deps: slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.ToolsByID = map[string]*domain.ToolDefinition{"book": tool}
		d.IntegrationsByID[integ.ID] = integ
	})}
	r.host.Register(r.hosted)
	r.host.SetUserDetailsLoader(func(context.Context, string) (domain.UserDetails, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return domain.UserDetails{Timezone: r.zone}, nil
	})
	if withLink {
		r.host.SetTimezoneLink(func(_ context.Context, userID, agentID, provider string) (string, error) {
			r.mu.Lock()
			r.links++
			r.mu.Unlock()
			if provider != "slack" { // the test conversation is on "slack": stands for a messenger that can show the button
				return "", nil
			}
			return "https://app.example/miniapp/timezone?agent=" + agentID, nil
		})
	}
	return r
}

func (r *zoneRig) say(t *testing.T, text string) {
	t.Helper()
	conv := domain.ConversationRef{Provider: "slack", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
	ctx := domain.WithOriginalMessage(context.Background(), text)
	if _, err := r.host.HandleTurn(ctx, r.hosted, conv, text); err != nil {
		t.Fatal(err)
	}
}

func (r *zoneRig) setZone(z string) { r.mu.Lock(); r.zone = z; r.mu.Unlock() }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Something that depends on the person's clock, with no time zone known: they are asked once, with
// one tap, what they asked is kept, nothing is booked in the meantime, and once the zone is set it is
// carried on and done.
func TestTimezone_ASlotNeedsTheirClockSoTheyAreAskedOnceAndItIsCarriedOnAfterwards(t *testing.T) {
	r := newZoneRig(t, true, "", true)
	r.say(t, "book a dentist appointment on Friday at 10")

	got := r.box.texts()
	if len(got) != 1 || !strings.Contains(got[0], "time zone") || strings.Contains(strings.ToLower(got[0]), "utc") {
		t.Fatalf("one plain prompt about their time zone: %q", got)
	}
	btn, _ := r.box.sent[0]["web_app_button"].(map[string]any)
	if btn["text"] != "Set my time zone" || btn["url"] != "https://app.example/miniapp/timezone?agent=a1" {
		t.Errorf("one tap: %v", r.box.sent[0]["web_app_button"])
	}
	if r.calendar.count() != 0 {
		t.Fatalf("nothing is booked at the wrong time while the zone is unknown: %d", r.calendar.count())
	}

	// The person sets it (the page saves it, then tells the host).
	r.setZone("Africa/Lagos")
	r.host.OnTimezoneSet(context.Background(), "u1")
	waitUntil(t, "the booking to be carried on", func() bool { return r.calendar.count() == 1 })

	// Telling it again does nothing: what was waiting was carried on once.
	r.host.OnTimezoneSet(context.Background(), "u1")
	time.Sleep(100 * time.Millisecond)
	if r.calendar.count() != 1 || len(r.box.texts()) != 1 {
		t.Errorf("carried on once, and no second prompt: bookings=%d texts=%q", r.calendar.count(), r.box.texts())
	}
}

// It does not ask when it need not: the zone is known, the tool does not depend on the clock, nobody is
// there to ask (a repeating task), or there is no way to ask (no link).
func TestTimezone_NeverAsksWhenItNeedNotOrCannot(t *testing.T) {
	cases := map[string]struct {
		needs   bool
		zone    string
		link    bool
		unatt   bool
		booking int
	}{
		"the zone is known":                     {true, "Europe/London", true, false, 1},
		"the tool does not depend on the clock": {false, "", true, false, 1},
		"no way to ask":                         {true, "", false, false, 1},
		"a repeating task has nobody to ask":    {true, "", true, true, 1},
	}
	for name, c := range cases {
		r := newZoneRig(t, c.needs, c.zone, c.link)
		if c.unatt {
			if err := r.host.RunScheduledTask(context.Background(), "a1", "u1", "id_slack", "C1", "book a slot"); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, name, func() bool { return r.calendar.count() == c.booking })
		} else {
			r.say(t, "book a slot")
			if r.calendar.count() != c.booking {
				t.Errorf("%s: it books as usual: %d", name, r.calendar.count())
			}
		}
		for _, text := range r.box.texts() {
			if strings.Contains(text, "time zone") {
				t.Errorf("%s: must not ask: %q", name, text)
			}
		}
		if r.links != 0 {
			t.Errorf("%s: no link was made: %d", name, r.links)
		}
	}
}

// Asking again right away (a second message before they tap) does not send a second prompt, but
// what they asked last is the one that is carried on.
func TestTimezone_AskedOnceInAShortWhileAndTheLatestRequestIsKept(t *testing.T) {
	r := newZoneRig(t, true, "", true)
	r.say(t, "book a haircut on Monday")
	r.say(t, "actually book it for Tuesday")
	if got := r.box.texts(); len(got) != 1 {
		t.Fatalf("one prompt, not two: %q", got)
	}
	r.setZone("Europe/Berlin")
	r.host.OnTimezoneSet(context.Background(), "u1")
	waitUntil(t, "the last request to be carried on", func() bool { return r.calendar.count() == 1 })
	if turns := r.llm.turns(); len(turns) != 3 || turns[2] != "actually book it for Tuesday" {
		t.Errorf("the latest request is the one carried on: %q", turns)
	}
}

// A messenger that cannot show the one-tap prompt is never asked on: nothing waits for an answer that
// cannot come, and the work goes ahead as it always did.
func TestTimezone_AMessengerThatCannotShowThePromptIsNotAsked(t *testing.T) {
	r := newZoneRig(t, true, "", true)
	conv := domain.ConversationRef{Provider: "discord", ChatID: "C1", IdentityID: "id_slack", UserID: "u1", SenderID: "C1"}
	ctx := domain.WithOriginalMessage(context.Background(), "book a slot")
	if _, err := r.host.HandleTurn(ctx, r.hosted, conv, "book a slot"); err != nil {
		t.Fatal(err)
	}
	if r.calendar.count() != 1 {
		t.Errorf("it books as before: %d", r.calendar.count())
	}
	for _, text := range r.box.texts() {
		if strings.Contains(text, "time zone") {
			t.Errorf("no prompt where it cannot be shown: %q", text)
		}
	}
	r.host.OnTimezoneSet(context.Background(), "u1") // nothing was held, so nothing happens
	time.Sleep(50 * time.Millisecond)
	if r.calendar.count() != 1 {
		t.Errorf("nothing to carry on: %d", r.calendar.count())
	}
}
