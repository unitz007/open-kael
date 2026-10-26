package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// FinishActionName is the always-available action a NativeLoop treats
// specially to terminate — the model must call it explicitly rather than
// the loop guessing "done" from plain text, mirroring the old framework's
// end_loop. Whoever builds the action list for a given Run call is
// responsible for including a BoundAction named FinishActionName with
// whatever InputSchema fits that call (free-text at an Agent's own top
// level, a Skill's OutputSchema when the loop is scoped to that Skill —
// see BindSkill).
const FinishActionName = "finish"

const (
	defaultMaxIterations   = 10
	llmMaxRetries          = 3
	llmBaseBackoff         = time.Second
	llmCooldown            = 60 * time.Second
	maxConsecutiveFailures = 3
	maxSameActionCalls     = 15
	maxToolCallsPerIter    = 10
	maxStaleSteps          = 2 // steps in a row with nothing new before a reply ends with what it has
)

// ErrSuperseded is what an action returns when the wait it was in (a question put to the person)
// was cut short because they wrote a new message instead of answering. When nothing has been
// changed yet this turn, the loop ends it quietly, keeping its history, and the new message is
// handled next with all of that in front of it. If something was already changed, the turn
// carries on so the person still hears what happened.
var ErrSuperseded = errors.New("the person wrote a new message instead")

type LoopStatus string

const (
	LoopStatusComplete     LoopStatus = "complete"
	LoopStatusMaxIteration LoopStatus = "max_iteration"
	LoopStatusError        LoopStatus = "error"
)

type LoopResult struct {
	Status  LoopStatus
	Content string
	// Output is set when the terminal "finish" call carried structured
	// arguments — the Skill-execution case (see BindSkill). Always the raw
	// arguments finish was called with, regardless of scope.
	Output map[string]any
	// Actions records what each action the loop ran really did (see OutcomeOf), so
	// callers can state facts about the turn rather than rely on the agent's words.
	Actions []ActionRecord
}

// Loop mirrors kael-platform's AgentLoop shape exactly (Run(ctx, messages,
// actions) -> (*LoopResult, updated messages, error)) — same reason as
// Message/ToolCall: it's a proven, simple contract. What's different is
// what it's reused FOR: the same Loop implementation runs an Agent's
// top-level reasoning AND a Skill's own internal tool orchestration, just
// handed a different action list and a different "finish" action.
type Loop interface {
	Run(ctx context.Context, messages []Message, actions []*BoundAction) (*LoopResult, []Message, error)
}

// NativeLoop is the default Loop — constructed automatically wherever
// Agent.Loop is nil (same nil-fallback pattern as the old framework's
// Agent.loop/SetLoop). It reuses the old nativeLoop's actual dispatch
// mechanics (explicit finish-to-terminate, duplicate/repeat-call guards,
// give-up-after-N-consecutive-failures), redirected at BoundAction instead
// of tools.ToolSpec, plus a real per-provider retry/backoff/cooldown
// circuit breaker across LLMs — not just try-next-on-error — matching the
// old framework's callLLM behavior.
type NativeLoop struct {
	LLMs          []LLM
	MaxIterations int

	mu        sync.Mutex
	downUntil map[int]time.Time
}

func NewNativeLoop(llms []LLM, maxIterations int) *NativeLoop {
	if maxIterations < 0 {
		maxIterations = 0
	}
	return &NativeLoop{
		LLMs:          llms,
		MaxIterations: maxIterations,
		downUntil:     make(map[int]time.Time),
	}
}

func availableNames(m map[string]*BoundAction) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

func (l *NativeLoop) Run(ctx context.Context, messages []Message, actions []*BoundAction) (*LoopResult, []Message, error) {
	ctx = WithRunJournal(ctx) // what runs successfully, for prerequisites (a nested run shares its caller's)
	actionsByName := make(map[string]*BoundAction, len(actions))
	specs := make([]ActionSpec, 0, len(actions))
	for _, a := range actions {
		actionsByName[a.Spec.Name] = a
		specs = append(specs, a.Spec)
	}

	callCounts := make(map[string]int)
	consecutiveFailures := 0

	// What each action really did, and — for a request to DO something — what must
	// have really happened before the agent may call it done.
	var records []ActionRecord
	// Changes that already went through this turn, by action and exact arguments. The same
	// change is never made twice in one turn, whatever makes the model ask again.
	doneChanges := map[string]string{}
	// Look-ups already made this turn, by action and exact arguments: the same question gets the same answer, without
	// asking the service again and without counting against anything. They replace per-ability call limits for
	// look-ups, which were guesses about how a task goes (comparing four players needs four look-ups).
	lookedUp := map[string]string{}
	// Steps in a row that ran nothing new (only repeats of what it already did): a reply that stops making progress ends.
	stale := 0
	reqs := RequirementsFromContext(ctx)
	if fin, ok := actionsByName[FinishActionName]; !reqs.Active() || !ok || !hasProperty(fin.Spec.InputSchema, "outcome") {
		// Only a loop whose finish asks for an outcome is held to the requirements, so
		// a nested capability loop sharing this context is not caught by them.
		reqs = nil
	}
	bounces := 0

	giveUp := func(reason string) (*LoopResult, []Message, error) {
		log.Printf("loop: gave up (%s) — what ran: %s", reason, describeRecords(records))
		return &LoopResult{Status: LoopStatusError, Content: reason, Actions: records}, messages, nil
	}
	// wrapUp ends a reply that can't usefully go on (it keeps failing or repeating itself, or has used its steps) with
	// an answer from what it found, instead of an error that throws all of that away: one more call, with only finish
	// offered. A "done" it can't back up is replaced by the facts, as at any finish.
	wrapUp := func(reason string) (*LoopResult, []Message, error) {
		fin, ok := actionsByName[FinishActionName]
		if !ok {
			return giveUp(reason)
		}
		log.Printf("loop: wrapping up (%s) — what ran: %s", reason, describeRecords(records))
		msgs := append(answerOpenCalls(messages), Message{Role: RoleUser, Internal: true, Content: wrapUpNote})
		resp, err := l.callLLM(ctx, msgs, []ActionSpec{fin.Spec})
		if err != nil || resp == nil || len(resp.ToolCalls) == 0 || resp.ToolCalls[0].Name != FinishActionName {
			return giveUp(reason)
		}
		call := resp.ToolCalls[0]
		content, _ := call.Arguments["content"].(string)
		if strings.TrimSpace(content) == "" {
			return giveUp(reason)
		}
		result := &LoopResult{Status: LoopStatusComplete, Content: content, Output: call.Arguments, Actions: records}
		if reqs != nil {
			if ok, _ := finishCheck(call.Arguments, reqs, records); !ok {
				result.Content = reqs.NothingChangedMessage(records)
			}
		}
		msgs = append(msgs, Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls[:1]},
			Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "done"})
		return result, msgs, nil
	}

	for i := 0; l.MaxIterations == 0 || i < l.MaxIterations; i++ {
		if stale >= maxStaleSteps {
			return wrapUp("no progress: it only repeated what it had already done")
		}
		progressed := false
		resp, err := l.callLLM(ctx, messages, specs)
		if err != nil {
			return nil, messages, fmt.Errorf("native loop: call llm: %w", err)
		}

		if len(resp.ToolCalls) == 0 {
			// Model produced no tool calls — nudge it to call finish or another action.
			messages = append(messages,
				Message{Role: RoleAssistant, Content: resp.Content, Internal: true},
				Message{Role: RoleUser, Content: fmt.Sprintf("You must respond by calling one of the available actions. Call %q to deliver your response to the user.", FinishActionName), Internal: true},
			)
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return wrapUp("too many responses with no action call")
			}
			continue
		}

		if len(resp.ToolCalls) > maxToolCallsPerIter {
			messages = append(messages,
				Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls[:0], Internal: true},
				Message{Role: RoleUser, Content: fmt.Sprintf("You requested %d tool calls at once. Make at most %d tool calls per response.", len(resp.ToolCalls), maxToolCallsPerIter), Internal: true},
			)
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return wrapUp("too many tool calls per response")
			}
			continue
		}

		messages = append(messages, Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		callNames := make([]string, len(resp.ToolCalls))
		for i, c := range resp.ToolCalls {
			callNames[i] = c.Name
		}
		log.Printf("loop: iter=%d tool_calls=%d names=%v", i, len(resp.ToolCalls), callNames)

		for idx, call := range resp.ToolCalls {
			if call.Name == FinishActionName {
				result := &LoopResult{Status: LoopStatusComplete, Output: call.Arguments, Actions: records}
				if content, ok := call.Arguments["content"].(string); ok {
					result.Content = content
				}
				if reqs != nil {
					ok, feedback := finishCheck(call.Arguments, reqs, records)
					if !ok {
						bounces++
						log.Printf("loop: finish refused (%d/%d): %s", bounces, maxFinishBounces, feedback)
						if bounces > maxFinishBounces {
							// The agent keeps claiming what didn't happen. Say what did.
							log.Printf("loop: finished — the agent kept claiming what didn't happen; stating the facts instead (what ran: %s)", describeRecords(records))
							result.Content = reqs.NothingChangedMessage(records)
							messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "done"})
							return result, messages, nil
						}
						messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: feedback})
						for _, rest := range resp.ToolCalls[idx+1:] { // every call needs an answer
							messages = append(messages, Message{Role: RoleTool, ToolCallID: rest.ID, Name: rest.Name, Content: "not run: finish was refused first"})
						}
						break // back to the model with the facts
					}
					outcome, _ := call.Arguments["outcome"].(string)
					log.Printf("loop: finished — outcome=%q, what ran: %s", outcome, describeRecords(records))
					if outcome == OutcomeDone {
						for _, line := range receipts(records) {
							result.Content += "\n\n" + line
						}
					}
				}
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "done"})
				return result, messages, nil
			}

			action, ok := actionsByName[call.Name]
			if !ok {
				log.Printf("loop: unknown action %q (available: %v)", call.Name, availableNames(actionsByName))
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: fmt.Sprintf("unknown action %q", call.Name)})
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return wrapUp("too many failed action calls")
				}
				continue
			}

			lookupKey := ""
			if action.Spec.ReadOnly {
				if args, merr := json.Marshal(call.Arguments); merr == nil {
					lookupKey = call.Name + "|" + string(args)
					if prev, repeated := lookedUp[lookupKey]; repeated {
						log.Printf("loop: %q asked again with the same inputs — answered with what it found before", call.Name)
						messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name,
							Content: "Already looked up in this reply, with the same inputs; the same as before:\n" + prev})
						continue
					}
				}
			}

			callCounts[call.Name]++
			if callCounts[call.Name] > maxSameActionCalls {
				// One reply may use an ability only so often. Reaching that is not a reason to throw away what it has
				// already found ("check all pairs" one pair at a time, 2026-10-25): it is told to answer with that and
				// say what it left out, and gives up only if it keeps asking.
				log.Printf("loop: action %q reached its %d uses for this reply — asking for an answer with what it has", call.Name, maxSameActionCalls)
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: overUsed(call.Name)})
				for _, rest := range resp.ToolCalls[idx+1:] { // every call needs an answer
					messages = append(messages, Message{Role: RoleTool, ToolCallID: rest.ID, Name: rest.Name, Content: "not run: " + overUsed(call.Name)})
				}
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return wrapUp(fmt.Sprintf("action %q called too many times", call.Name))
				}
				break
			}
			if !action.Spec.ReadOnly && action.Spec.MaxCalls > 0 && callCounts[call.Name] > action.Spec.MaxCalls {
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name,
					Content: fmt.Sprintf("already called %d time(s) this turn — the result is already in context, do not call again", action.Spec.MaxCalls)})
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return wrapUp("too many failed action calls")
				}
				continue
			}

			changeKey := ""
			if action.Spec.Changes {
				if args, merr := json.Marshal(call.Arguments); merr == nil {
					changeKey = call.Name + "|" + string(args)
					if prev, repeated := doneChanges[changeKey]; repeated {
						log.Printf("loop: action %q already made this exact change this turn — not repeating it", call.Name)
						messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name,
							Content: "already done this turn, so it was not repeated. Result: " + prev})
						continue
					}
				}
			}

			noteStep(ctx, call.Name, StepRunning)
			progressed = true // something new ran (a failure is counted by its own rule)
			output, err := action.Invoke(ctx, call.Arguments)
			noteStep(ctx, call.Name, StepStateOf(output, err))
			done, msg, why := OutcomeOf(output, err)
			records = append(records, ActionRecord{Name: call.Name, Changes: action.Spec.Changes, Done: done, Message: msg, Reason: why})
			if errors.Is(err, ErrSuperseded) && !changedAnything(records) {
				log.Printf("loop: the person wrote a new message while waiting — ending this turn quietly; their message is handled next")
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "not answered: the person wrote a new message instead"})
				for _, rest := range resp.ToolCalls[idx+1:] { // every call needs an answer
					messages = append(messages, Message{Role: RoleTool, ToolCallID: rest.ID, Name: rest.Name, Content: "not run: the person wrote a new message first"})
				}
				return &LoopResult{Status: LoopStatusComplete, Actions: records}, messages, nil
			}
			if err != nil {
				log.Printf("loop: action %q failed: %v", call.Name, err)
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: fmt.Sprintf("error: %v", err)})
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return wrapUp("too many failed action calls")
				}
				continue
			}

			consecutiveFailures = 0
			if changeKey != "" && done {
				doneChanges[changeKey] = truncateRunes(StringifyResult(output), 300)
			}
			if lookupKey != "" && done {
				lookedUp[lookupKey] = StringifyResult(output)
			}
			messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: StringifyResult(output)})
		}
		if progressed {
			stale = 0
		} else {
			stale++
		}
	}

	if _, ok := actionsByName[FinishActionName]; ok {
		return wrapUp("used all its steps")
	}
	return &LoopResult{Status: LoopStatusMaxIteration}, messages, nil
}

// wrapUpNote asks a reply that is being cut short for its answer.
const wrapUpNote = "Stop using abilities now. Answer the person with what you found so far, and say plainly what you couldn't finish (they can ask again). Don't say anything was done unless it really went through. Call finish."

// answerOpenCalls answers any call of the last assistant message that has no answer yet (a reply cut short in the
// middle of several calls), so the conversation stays well formed for one more call to the model.
func answerOpenCalls(messages []Message) []Message {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant && len(messages[i].ToolCalls) > 0 {
			last = i
			break
		}
	}
	if last < 0 {
		return messages
	}
	answered := map[string]bool{}
	for _, m := range messages[last+1:] {
		if m.Role == RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	out := append([]Message(nil), messages...)
	for _, c := range messages[last].ToolCalls {
		if !answered[c.ID] {
			out = append(out, Message{Role: RoleTool, ToolCallID: c.ID, Name: c.Name, Content: "not run: the reply is wrapping up"})
		}
	}
	return out
}

// overUsed is what an ability that reached its uses for one reply answers instead of running.
func overUsed(name string) string {
	return fmt.Sprintf("not run: %s has been used %d times in this reply, as many as one reply may use. Answer now with what you already have, and say plainly what you did not get to (the person can ask for the rest).", name, maxSameActionCalls)
}

// callLLM tries each provider in priority order. A provider that exhausts
// its retries is marked down for a cooldown window and skipped by later
// calls — except the last remaining provider, which is always tried since
// there's nothing further to fall back to. Mirrors the old framework's
// callLLM circuit breaker.
func (l *NativeLoop) callLLM(ctx context.Context, messages []Message, specs []ActionSpec) (*LLMResponse, error) {
	if len(l.LLMs) == 0 {
		return nil, fmt.Errorf("no LLM configured")
	}

	var lastErr error
	for i, provider := range l.LLMs {
		last := i == len(l.LLMs)-1
		if !last && l.isDown(i) {
			continue
		}

		resp, err := l.callWithRetry(ctx, provider, messages, specs)
		if err == nil {
			l.markUp(i)
			return resp, nil
		}
		lastErr = err
		if !last {
			l.markDown(i)
		}
	}
	return nil, fmt.Errorf("all llm providers failed: %w", lastErr)
}

func (l *NativeLoop) callWithRetry(ctx context.Context, provider LLM, messages []Message, specs []ActionSpec) (*LLMResponse, error) {
	backoff := llmBaseBackoff
	var lastErr error
	for attempt := 0; attempt < llmMaxRetries; attempt++ {
		resp, err := provider.Call(ctx, messages, specs)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if errors.Is(err, ErrLLMOutOfCredit) {
			// Asking again cannot help until the account is topped up: say so at once, and let the
			// next provider (if there is one) have a go.
			break
		}
		if attempt == llmMaxRetries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, lastErr
}

func (l *NativeLoop) isDown(i int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Now().Before(l.downUntil[i])
}

func (l *NativeLoop) markDown(i int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.downUntil[i] = time.Now().Add(llmCooldown)
}

func (l *NativeLoop) markUp(i int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.downUntil, i)
}

func StringifyResult(output any) string {
	if s, ok := output.(string); ok {
		return s
	}
	if b, err := json.Marshal(output); err == nil {
		return string(b)
	}
	return fmt.Sprint(output)
}

// hasProperty reports whether an object schema declares the named property.
func hasProperty(s Schema, name string) bool {
	_, ok := s.Properties[name]
	return ok
}

// truncateRunes keeps the first n characters of s.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// changedAnything reports whether any action that changes something has really succeeded.
func changedAnything(records []ActionRecord) bool {
	for _, r := range records {
		if r.Changes && r.Done {
			return true
		}
	}
	return false
}
