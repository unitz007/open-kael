package runtime

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/unitz007/open-kael/domain"
)

// ── Asking the person to pick one of a few options ────────────────────────────
//
// The same mechanism as approvals (a prompt with buttons, a token, a wait with a timeout, a
// callback that resolves it), with one button per option instead of approve/reject.

// AskOptionsActionName is the name of the action the agent calls to ask a multiple-choice question.
const AskOptionsActionName = "ask_options"

const (
	minChoiceOptions  = 2
	maxChoiceOptions  = 6
	maxChoiceQuestion = 300
	maxChoiceLabel    = 40
	maxQuestionsAsked = 3 // per turn: a person is asked, not interrogated

	// choiceTimeoutSeconds is how long a question waits for a tap. Short on purpose: an agent
	// handles one message at a time, so while a question waits, whatever the person types instead
	// (and every other person talking to the agent) queues behind it.
	choiceTimeoutSeconds = 120
)

// TenantChoiceRequester is the identity-aware counterpart to domain.ChoiceRequester, like
// TenantApprovalRequester: one shared Executor serves many bots, so which credentials to use
// is bound per turn.
type TenantChoiceRequester interface {
	RequestChoice(ctx context.Context, identity *domain.Identity, connectionRef string, conv domain.ConversationRef, question string, options []string, timeoutSeconds int) (domain.ChoiceAnswer, error)
}

type choiceRequesterFunc func(ctx context.Context, conv domain.ConversationRef, question string, options []string, timeoutSeconds int) (domain.ChoiceAnswer, error)

func (f choiceRequesterFunc) RequestChoice(ctx context.Context, conv domain.ConversationRef, question string, options []string, timeoutSeconds int) (domain.ChoiceAnswer, error) {
	return f(ctx, conv, question, options, timeoutSeconds)
}

// resolveChoiceRequester finds conv's bot Identity/Executor and, if that Executor can ask
// multiple-choice questions, returns a domain.ChoiceRequester ready to attach to ctx.
func resolveChoiceRequester(hosted *HostedAgent, conv domain.ConversationRef) (domain.ChoiceRequester, bool) {
	identity, integration := resolveIdentityAndIntegration(hosted, conv)
	if integration == nil {
		return nil, false
	}
	executor, ok := hosted.Deps.Executors.For(integration.Service)
	if !ok {
		return nil, false
	}
	if tenant, ok := executor.(TenantChoiceRequester); ok && identity != nil {
		return choiceRequesterFunc(func(ctx context.Context, conv domain.ConversationRef, question string, options []string, timeoutSeconds int) (domain.ChoiceAnswer, error) {
			connectionRef, _ := domain.ConnectionRefFromContext(ctx, identity.ID)
			return tenant.RequestChoice(ctx, identity, connectionRef, conv, question, options, timeoutSeconds)
		}), true
	}
	if requester, ok := executor.(domain.ChoiceRequester); ok {
		return requester, true
	}
	return nil, false
}

// MessengerChoiceRequester is the generic orchestration for choice prompts, on top of a
// messenger that can show buttons (domain.ChoiceMessenger): a token per question, a wait
// channel per pending one, a timeout.
type MessengerChoiceRequester struct {
	Messenger domain.InteractiveMessenger

	mu      sync.Mutex
	waiters map[string]chan domain.ChoiceAnswer
	byConv  map[string]string // conversation → the token of the question waiting in it
	seq     atomic.Uint64
}

func NewMessengerChoiceRequester(messenger domain.InteractiveMessenger) *MessengerChoiceRequester {
	return &MessengerChoiceRequester{Messenger: messenger, waiters: make(map[string]chan domain.ChoiceAnswer), byConv: make(map[string]string)}
}

var _ TenantChoiceRequester = (*MessengerChoiceRequester)(nil)

// RequestChoice posts the question with one button per option plus "something else", then
// blocks until the person taps one or the timeout passes (defaultApprovalTimeout when <= 0).
// It returns the position of the chosen option; len(options) means "something else".
func (r *MessengerChoiceRequester) RequestChoice(ctx context.Context, identity *domain.Identity, connectionRef string, conv domain.ConversationRef, question string, options []string, timeoutSeconds int) (domain.ChoiceAnswer, error) {
	cm, ok := r.Messenger.(domain.ChoiceMessenger)
	if !ok {
		return domain.ChoiceAnswer{}, fmt.Errorf("request choice: this messenger can't show choices")
	}
	token := fmt.Sprintf("choice-%d", r.seq.Add(1))
	ch := make(chan domain.ChoiceAnswer, 1)
	key := conversationKey(conv)

	r.mu.Lock()
	r.waiters[token] = ch
	r.byConv[key] = token
	r.mu.Unlock()
	cleanup := func() {
		r.mu.Lock()
		delete(r.waiters, token)
		if r.byConv[key] == token {
			delete(r.byConv, key)
		}
		r.mu.Unlock()
	}

	buttons := append(append([]string(nil), options...), domain.ChoiceOtherLabel)
	messageID, err := cm.PostChoicePrompt(ctx, identity, connectionRef, conv, question, buttons, token)
	if err != nil {
		cleanup()
		return domain.ChoiceAnswer{}, fmt.Errorf("request choice: post prompt: %w", err)
	}

	timeout := defaultApprovalTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case ans := <-ch:
		shown := domain.ChoiceOtherLabel
		switch {
		case ans.Index == domain.ChoiceSuperseded:
			shown = "⏭ Replaced by your new message"
		case ans.Index >= 0 && ans.Index < len(options):
			shown = options[ans.Index]
		}
		mark := "✅ "
		if ans.Index == domain.ChoiceSuperseded {
			mark = ""
		}
		_ = r.Messenger.UpdateMessage(context.Background(), identity, connectionRef, conv, messageID, question+"\n\n"+mark+shown)
		return ans, nil
	case <-waitCtx.Done():
		cleanup()
		_ = r.Messenger.UpdateMessage(context.Background(), identity, connectionRef, conv, messageID, question+"\n\n⏱ No answer.")
		return domain.ChoiceAnswer{}, waitCtx.Err()
	}
}

// Resolve is called by whatever receives the provider's button callback, with the token and
// the position of the button tapped. It reports false when no wait matches the token (already
// answered, timed out, or from before a restart).
func (r *MessengerChoiceRequester) Resolve(token string, index int) bool {
	return r.resolve(token, domain.ChoiceAnswer{Index: index})
}

func (r *MessengerChoiceRequester) resolve(token string, ans domain.ChoiceAnswer) bool {
	r.mu.Lock()
	ch, ok := r.waiters[token]
	if ok {
		delete(r.waiters, token)
		for k, t := range r.byConv {
			if t == token {
				delete(r.byConv, k)
			}
		}
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	ch <- ans
	return true
}

// Supersede cuts short the question waiting in this conversation, because the person wrote a new
// message instead of answering. The question's turn ends and the new message is handled next,
// with the unanswered question in front of it, so it is understood as an answer or as something
// new. It reports whether a question was waiting.
func (r *MessengerChoiceRequester) Supersede(conv domain.ConversationRef) bool {
	r.mu.Lock()
	token, ok := r.byConv[conversationKey(conv)]
	r.mu.Unlock()
	if !ok {
		return false
	}
	return r.resolve(token, domain.ChoiceAnswer{Index: domain.ChoiceSuperseded})
}

// conversationKey identifies one person's conversation in one chat of one bot: the unit that is
// handled one message at a time, and the place a waiting question belongs to.
func conversationKey(c domain.ConversationRef) string {
	return c.IdentityID + "|" + c.ChatID + "|" + c.ThreadID + "|" + c.SenderID
}

// askOptionsAction is the action the agent calls to put a short multiple-choice question to
// the person. It only reads their answer: it changes nothing, so it never counts toward
// "the action was really done", and it can be called at most a few times in a turn.
func askOptionsAction(requester domain.ChoiceRequester) *domain.BoundAction {
	return &domain.BoundAction{
		Spec: domain.ActionSpec{
			Name: AskOptionsActionName,
			Description: "Ask the person to pick one of a few options, shown as buttons, and wait for their tap. " +
				"Use it when you need their decision and there are 2 to 6 short, distinct choices (for example which of several matches they meant, or a size to go with). " +
				"Do not use it for something they already told you, for a name or number they can simply type, or to confirm an action that already asks for approval. " +
				"Ask one question at a time and never the same question twice: once they have answered, use the answer. The result is the option they chose; if they chose 'Something else' or did not answer, say so and ask in your own words.",
			MaxCalls: maxQuestionsAsked,
			InputSchema: domain.Schema{
				Type: domain.SchemaTypeObject,
				Properties: map[string]domain.Schema{
					"question": {Type: domain.SchemaTypeString, Description: "The question, in the person's language, one short sentence."},
					"options": {
						Type:        domain.SchemaTypeArray,
						Description: "2 to 6 short, distinct answers, each a few words.",
						Items:       &domain.Schema{Type: domain.SchemaTypeString},
					},
				},
				Required: []string{"question", "options"},
			},
		},
		Invoke: func(ctx context.Context, input map[string]any) (any, error) {
			question, options, err := parseChoiceInput(input)
			if err != nil {
				return nil, err
			}
			conv, ok := domain.ConversationFromContext(ctx)
			if !ok {
				return map[string]any{"answer": "", "status": "no_answer", "note": "There is no conversation to ask in. Ask in your own words."}, nil
			}
			ans, rerr := requester.RequestChoice(ctx, conv, question, options, choiceTimeoutSeconds)
			if rerr != nil {
				log.Printf("runtime: ask_options: %v", rerr)
				return map[string]any{"answer": "", "status": "no_answer", "note": "The person did not answer. Carry on without it, or say you need it."}, nil
			}
			if ans.Index == domain.ChoiceSuperseded {
				return nil, domain.ErrSuperseded
			}
			if ans.Index < 0 || ans.Index >= len(options) {
				return map[string]any{"answer": "", "status": "other", "note": "The person chose 'Something else'. Ask them in a short message what they would like instead."}, nil
			}
			return map[string]any{"answer": options[ans.Index], "position": ans.Index + 1, "status": "answered"}, nil
		},
	}
}

// parseChoiceInput checks what the model sent: a short question and 2 to 6 distinct, short options.
func parseChoiceInput(input map[string]any) (string, []string, error) {
	question, _ := input["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" || utf8.RuneCountInString(question) > maxChoiceQuestion {
		return "", nil, fmt.Errorf("ask_options: give a question of up to %d characters", maxChoiceQuestion)
	}
	raw, _ := input["options"].([]any)
	if strs, ok := input["options"].([]string); ok {
		for _, s := range strs {
			raw = append(raw, s)
		}
	}
	seen := map[string]bool{}
	var options []string
	for _, v := range raw {
		s, _ := v.(string)
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		if utf8.RuneCountInString(s) > maxChoiceLabel {
			return "", nil, fmt.Errorf("ask_options: each option must be short (up to %d characters)", maxChoiceLabel)
		}
		seen[strings.ToLower(s)] = true
		options = append(options, s)
	}
	if len(options) < minChoiceOptions || len(options) > maxChoiceOptions {
		return "", nil, fmt.Errorf("ask_options: give %d to %d different options", minChoiceOptions, maxChoiceOptions)
	}
	return question, options, nil
}
