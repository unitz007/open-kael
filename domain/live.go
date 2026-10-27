package domain

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// Live: what the person sees while a reply is being worked out. A chat app that can show it is told which of the
// agent's abilities the model's thinking is weighing (only their plain names, never its words: its thinking calls them
// by their model names and talks about the person and its instructions) and the reply as the model writes it.

// LiveShower is a chat app that shows the person, while the agent works, which abilities it is thinking about and its
// reply as it is written. Optional (beside StepShower); no call may block.
type LiveShower interface {
	// ThinkingAbout names the abilities (plain names) the model's thinking has named in the step it is on; none when a
	// new step begins.
	ThinkingAbout(conv ConversationRef, abilities []string)
	// ShowDraft is the reply so far, as the model writes it. The reply that is sent at the end replaces it, and a turn
	// that ends without a reply (an error said another way, a sign-in) takes it away.
	ShowDraft(conv ConversationRef, text string)
}

// CallWatcher hears a model's answer as it is written, for the calls made with a context that carries it (an LLM that
// can stream tells it; one that can't simply doesn't). No call may block.
type CallWatcher interface {
	CallBegins()                                     // a new call (or another try of one): forget what came before
	Thinking(delta string)                           // more of the model's thinking
	ToolCall(index int, name, argumentsDelta string) // more of one action call's arguments (name only on its first piece)
}

type callWatcherKey struct{}

// WithCallWatcher makes the model calls made with ctx tell w what is written as it comes; nil takes it away.
func WithCallWatcher(ctx context.Context, w CallWatcher) context.Context {
	return context.WithValue(ctx, callWatcherKey{}, w)
}

// CallWatcherFrom returns the watcher the model calls made with ctx should tell, if any.
func CallWatcherFrom(ctx context.Context) (CallWatcher, bool) {
	w, ok := ctx.Value(callWatcherKey{}).(CallWatcher)
	return w, ok && w != nil
}

type replyWatchKey struct{}

// WithReplyWatch attaches the watcher for this turn's reply. Only the agent loop's own calls tell it (not the loops an
// action runs, nor the calls made around the turn), and a reply checked before it is sent (a request to do something,
// held to what really happened) only tells it the thinking.
func WithReplyWatch(ctx context.Context, w CallWatcher) context.Context {
	return context.WithValue(ctx, replyWatchKey{}, w)
}

func replyWatchFrom(ctx context.Context) CallWatcher {
	w, _ := ctx.Value(replyWatchKey{}).(CallWatcher)
	return w
}

// thinkingOnly passes on the thinking and nothing of the reply.
type thinkingOnly struct{ CallWatcher }

func (thinkingOnly) ToolCall(int, string, string) {}

// WatchReply is the CallWatcher for a reply: it tells about which of abilities (model name → plain name) the model's
// thinking names, in the step it is on, and draft the finish action's content as it grows. Either may be nil.
func WatchReply(abilities map[string]string, about func([]string), draft func(string)) CallWatcher {
	w := &replyWatch{plain: abilities, about: about, draft: draft}
	// A name like get_my_team is only ever the ability. One that is also a word ("directions") counts only where it is
	// written as a name, `directions` or directions(: in a sentence it is just the word.
	var named, worded []string
	for n := range abilities {
		if strings.ContainsAny(n, "_-0123456789") {
			named = append(named, regexp.QuoteMeta(n))
		} else {
			worded = append(worded, regexp.QuoteMeta(n))
		}
	}
	var parts []string
	if len(named) > 0 {
		parts = append(parts, `\b(?:`+strings.Join(named, "|")+`)\b`)
	}
	if len(worded) > 0 {
		parts = append(parts, "`(?:"+strings.Join(worded, "|")+")`", `\b(?:`+strings.Join(worded, "|")+`)\(`)
	}
	if len(parts) > 0 {
		w.names = regexp.MustCompile(strings.Join(parts, "|"))
	}
	return w
}

type replyWatch struct {
	plain map[string]string
	names *regexp.Regexp
	about func([]string)
	draft func(string)

	mu       sync.Mutex
	thought  strings.Builder
	tail     string // the end of the thinking already searched, so a name split across pieces is still found
	named    []string
	seen     map[string]bool
	calls    map[int]string           // index → action name
	args     map[int]*strings.Builder // index → its arguments so far
	lastText string
}

func (w *replyWatch) CallBegins() {
	w.mu.Lock()
	hadNames := len(w.named) > 0
	w.thought.Reset()
	w.tail, w.named, w.seen = "", nil, map[string]bool{}
	w.calls, w.args = map[int]string{}, map[int]*strings.Builder{}
	w.mu.Unlock()
	if hadNames && w.about != nil {
		w.about(nil)
	}
}

func (w *replyWatch) Thinking(delta string) {
	if w.names == nil || w.about == nil || delta == "" {
		return
	}
	w.mu.Lock()
	text := w.tail + delta
	var found []string
	for _, m := range w.names.FindAllString(text, -1) {
		if p := w.plain[strings.Trim(m, "`(")]; p != "" && !w.seen[p] {
			w.seen[p] = true
			w.named = append(w.named, p)
			found = w.named
		}
	}
	if len(text) > 80 {
		text = text[len(text)-80:]
	}
	w.tail = text
	named := append([]string(nil), found...)
	w.mu.Unlock()
	if len(named) > 0 {
		w.about(named)
	}
}

func (w *replyWatch) ToolCall(index int, name, delta string) {
	if w.draft == nil {
		return
	}
	w.mu.Lock()
	if w.calls == nil {
		w.calls, w.args = map[int]string{}, map[int]*strings.Builder{}
	}
	if name != "" {
		w.calls[index] = name
	}
	if w.calls[index] != FinishActionName {
		w.mu.Unlock()
		return
	}
	b := w.args[index]
	if b == nil {
		b = &strings.Builder{}
		w.args[index] = b
	}
	b.WriteString(delta)
	text, ok := PartialStringField(b.String(), "content")
	if !ok || text == w.lastText {
		w.mu.Unlock()
		return
	}
	w.lastText = text
	w.mu.Unlock()
	w.draft(text)
}

// PartialStringField reads the value of a top-level string field from JSON that is still being written: what has
// arrived of it so far, unescaped. False until the field's value has begun. Arguments sent as a JSON string holding the
// JSON (some providers stream them that way) are read too.
func PartialStringField(partial, field string) (string, bool) {
	s := strings.TrimLeft(partial, " \t\r\n")
	if strings.HasPrefix(s, `"`) { // the whole object as a string: read the string, then its contents
		inner, _ := partialString(s[1:])
		s = strings.TrimLeft(inner, " \t\r\n")
	}
	if !strings.HasPrefix(s, "{") {
		return "", false
	}
	// Walk the top level: keys and values, skipping nested values, until the field.
	i, depth := 1, 1
	for i < len(s) {
		c := s[i]
		switch {
		case c == '"':
			str, end := scanString(s, i+1)
			if end < 0 {
				return "", false
			}
			i = end + 1
			if depth == 1 && str == field {
				j := i
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
					j++
				}
				if j >= len(s) || s[j] != ':' {
					continue // a value equal to the field's name, not the key
				}
				j++
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
					j++
				}
				if j >= len(s) || s[j] != '"' {
					return "", false
				}
				v, _ := partialString(s[j+1:])
				return v, true
			}
		case c == '{' || c == '[':
			depth++
			i++
		case c == '}' || c == ']':
			depth--
			i++
		default:
			i++
		}
	}
	return "", false
}

// scanString finds where a JSON string that starts at from ends (the index of its closing quote, -1 if not yet), and
// its value.
func scanString(s string, from int) (string, int) {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var v string
			if err := json.Unmarshal([]byte(s[from-1:i+1]), &v); err != nil {
				return s[from:i], i
			}
			return v, i
		}
	}
	return "", -1
}

// partialString unescapes a JSON string's contents that may not have ended yet, leaving out an escape cut short.
func partialString(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), true
		case c != '\\':
			b.WriteByte(c)
		case i+1 >= len(s):
			return b.String(), false
		default:
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b', 'f':
			case 'u':
				if i+4 >= len(s) {
					return b.String(), false
				}
				esc := `\u` + s[i+1:i+5]
				if hi := strings.ToLower(s[i+1 : i+3]); hi >= "d8" && hi <= "db" { // half of a pair (an emoji): wait for both
					if i+10 >= len(s) {
						return b.String(), false
					}
					esc += s[i+5 : i+11]
					i += 6
				}
				var r string
				if err := json.Unmarshal([]byte(`"`+esc+`"`), &r); err == nil {
					b.WriteString(r)
				}
				i += 4
			default: // \" \\ \/
				b.WriteByte(s[i])
			}
		}
	}
	return b.String(), false
}
