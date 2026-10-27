package domain_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// The reply's words are read from the finish call's arguments as they arrive, however they are cut.
func TestPartialStringField_ReadsTheReplyAsItArrives(t *testing.T) {
	full := `{"outcome":"answered","content":"Captain \"Haaland\".\nVice: Saka 😀 — fine","choices":[{"label":"x","say":"content"}]}`
	var last string
	for i := 0; i <= len(full); i++ {
		got, ok := domain.PartialStringField(full[:i], "content")
		if !ok {
			if strings.Contains(full[:i], `"content":"C`) {
				t.Fatalf("not read at %d: %q", i, full[:i])
			}
			continue
		}
		if !strings.HasPrefix("Captain \"Haaland\".\nVice: Saka 😀 — fine", got) && got != "" {
			t.Fatalf("at %d read %q", i, got)
		}
		if len(got) < len(last) {
			t.Fatalf("went backwards at %d: %q after %q", i, got, last)
		}
		last = got
	}
	if last != "Captain \"Haaland\".\nVice: Saka 😀 — fine" {
		t.Fatalf("ended with %q", last)
	}
	// Escaped emoji (a pair) are only written once both halves are in.
	esc := `{"content":"ok 😀!"}`
	for i := range esc {
		if got, _ := domain.PartialStringField(esc[:i], "content"); strings.ContainsRune(got, '�') {
			t.Fatalf("half an emoji at %d: %q", i, got)
		}
	}
	if got, _ := domain.PartialStringField(esc, "content"); got != "ok 😀!" {
		t.Fatalf("escaped emoji: %q", got)
	}
	// A field of that name inside something nested, or as a value, is not the reply.
	if got, ok := domain.PartialStringField(`{"choices":[{"content":"no"}],"x":"content","content":"yes`, "content"); !ok || got != "yes" {
		t.Fatalf("nested or as a value: %q %v", got, ok)
	}
	// Arguments sent as a string of JSON.
	if got, ok := domain.PartialStringField(`"{\"content\":\"Hi the`, "content"); !ok || got != "Hi the" {
		t.Fatalf("as a string: %q %v", got, ok)
	}
}

type heard struct {
	mu     sync.Mutex
	about  [][]string
	drafts []string
}

func (h *heard) watch() domain.CallWatcher {
	return domain.WatchReply(map[string]string{"get_my_team": "My Team", "get_fixtures": "Fixtures"},
		func(a []string) { h.mu.Lock(); h.about = append(h.about, a); h.mu.Unlock() },
		func(s string) { h.mu.Lock(); h.drafts = append(h.drafts, s); h.mu.Unlock() })
}

// Only the names of abilities are picked out of the model's thinking (never its words), and a name cut in two
// between pieces is still found.
func TestWatchReply_NamesTheAbilitiesItsThinkingWeighs(t *testing.T) {
	h := &heard{}
	w := h.watch()
	w.CallBegins()
	for _, piece := range []string{"I should look at the user's team first: get_my", "_team. Then get_fixtures for the gameweek. get_my_team again."} {
		w.Thinking(piece)
	}
	if len(h.about) != 1 || strings.Join(h.about[0], ",") != "My Team,Fixtures" {
		t.Fatalf("about: %v", h.about)
	}
	// A name that is also a word counts only where it is written as a name.
	h2 := &heard{}
	w2 := domain.WatchReply(map[string]string{"directions": "Directions", "weather_forecast": "Weather Forecast"},
		func(a []string) { h2.about = append(h2.about, a) }, nil)
	w2.CallBegins()
	w2.Thinking("It can find places and give directions to them. I'll use weather_forecast")
	w2.Thinking(" and then `directions` for the walk.")
	if len(h2.about) != 2 || strings.Join(h2.about[1], ",") != "Weather Forecast,Directions" {
		t.Fatalf("a word is not a name: %v", h2.about)
	}

	w.CallBegins() // a new step: nothing named in it yet
	if last := h.about[len(h.about)-1]; len(last) != 0 {
		t.Fatalf("a new step starts with nothing named: %v", h.about)
	}
	w.ToolCall(0, "get_my_team", `{}`)
	w.ToolCall(1, domain.FinishActionName, `{"content":"Capt`)
	w.ToolCall(1, "", `ain Haaland"}`)
	if strings.Join(h.drafts, "|") != "Capt|Captain Haaland" {
		t.Fatalf("drafts: %v", h.drafts)
	}
}

// watchingLLM stands in for a model that writes as it goes.
type watchingLLM struct {
	calls func(n int) (thinking string, resp *domain.LLMResponse)
	n     int
}

func (l *watchingLLM) Call(ctx context.Context, _ []domain.Message, _ []domain.ActionSpec) (*domain.LLMResponse, error) {
	l.n++
	thinking, resp := l.calls(l.n)
	if w, ok := domain.CallWatcherFrom(ctx); ok {
		w.CallBegins()
		w.Thinking(thinking)
		for i, c := range resp.ToolCalls {
			w.ToolCall(i, c.Name, `{"content":"`+strings.ReplaceAll(c.Arguments["content"].(string), `"`, `\"`)+`"}`)
		}
	}
	return resp, nil
}

func finishWith(text string) *domain.LLMResponse {
	return &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "f", Name: domain.FinishActionName, Arguments: map[string]any{"content": text, "outcome": "answered"}}}}
}

var replyFinish = &domain.BoundAction{
	Spec: domain.ActionSpec{Name: domain.FinishActionName, InputSchema: domain.Schema{Type: "object", Properties: map[string]domain.Schema{
		"content": {Type: "string"}, "outcome": {Type: "string"}}}},
	Invoke: func(context.Context, map[string]any) (any, error) { return nil, nil },
}

// The agent loop tells the turn's watcher its own calls only: a loop run by one of its actions writes no draft of the
// reply. And a reply checked before it is sent (a request to do something) shows only what it is thinking about.
func TestNativeLoop_TellsTheReplyWatchOfItsOwnCallsOnly(t *testing.T) {
	inner := domain.NewNativeLoop([]domain.LLM{&watchingLLM{calls: func(int) (string, *domain.LLMResponse) {
		return "", finishWith("an inner answer, not the reply")
	}}}, 3)
	nested := &domain.BoundAction{
		Spec: domain.ActionSpec{Name: "get_my_team", ReadOnly: true},
		Invoke: func(ctx context.Context, _ map[string]any) (any, error) {
			if _, ok := domain.CallWatcherFrom(ctx); ok {
				t.Error("an action's context carries the watcher")
			}
			res, _, err := inner.Run(ctx, nil, []*domain.BoundAction{replyFinish})
			return res.Content, err
		},
	}
	newOuter := func() *domain.NativeLoop {
		return domain.NewNativeLoop([]domain.LLM{&watchingLLM{calls: func(n int) (string, *domain.LLMResponse) {
			if n == 1 {
				return "look at get_my_team", &domain.LLMResponse{ToolCalls: []domain.ToolCall{{ID: "1", Name: "get_my_team", Arguments: map[string]any{"content": ""}}}}
			}
			return "", finishWith(`Captain "Haaland"`)
		}}}, 5)
	}

	h := &heard{}
	ctx := domain.WithReplyWatch(context.Background(), h.watch())
	res, _, err := newOuter().Run(ctx, nil, []*domain.BoundAction{nested, replyFinish})
	if err != nil || res.Content != `Captain "Haaland"` {
		t.Fatalf("run: %v %+v", err, res)
	}
	if strings.Join(h.drafts, "|") != `Captain "Haaland"` {
		t.Fatalf("drafts: %q", h.drafts)
	}
	if len(h.about) == 0 || strings.Join(h.about[0], ",") != "My Team" {
		t.Fatalf("about: %v", h.about)
	}

	h = &heard{}
	ctx = domain.WithRequirements(domain.WithReplyWatch(context.Background(), h.watch()), &domain.Requirements{AnyChange: true})
	_, _, _ = newOuter().Run(ctx, nil, []*domain.BoundAction{nested, replyFinish})
	if len(h.drafts) != 0 || len(h.about) == 0 {
		t.Fatalf("a checked reply shows no draft, but still what it thinks about: %v %v", h.drafts, h.about)
	}
}
