package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// FinishActionName is the always-available action a NativeLoop treats
// specially to terminate — the model must call it explicitly rather than
// the loop guessing "done" from plain text. Whoever builds the action list
// for a given Run call is responsible for including a BoundAction named
// FinishActionName with whatever InputSchema fits that call (free-text at
// an Agent's own top level, a Skill's OutputSchema when the loop is scoped
// to that Skill — see BindSkill).
const FinishActionName = "finish"

const (
	defaultMaxIterations   = 10
	llmMaxRetries          = 3
	llmBaseBackoff         = time.Second
	llmCooldown            = 60 * time.Second
	maxConsecutiveFailures = 3
	maxSameActionCalls     = 5
)

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
}

// Loop mirrors kael-platform's AgentLoop shape exactly (Run(ctx, messages,
// actions) -> (*LoopResult, updated messages, error)) — same reason as
// Message/ToolCall: it's a proven, simple contract. What's different is
// what it's reused FOR: the same Loop implementation runs an Agent's
// top-level reasoning AND a Skill's own internal tool orchestration, just
// handed a different action list and a different "finish" action.
//
// Not used by the Sports Agent pilot — see NewNativeLoop's doc comment.
type Loop interface {
	Run(ctx context.Context, messages []Message, actions []*BoundAction) (*LoopResult, []Message, error)
}

// NativeLoop is the default Loop — constructed automatically wherever
// Agent.Loop is nil. It reuses kael-platform's own nativeLoop's actual
// dispatch mechanics (explicit finish-to-terminate, duplicate/repeat-call
// guards, give-up-after-N-consecutive-failures), redirected at BoundAction
// instead of tools.ToolSpec, plus a real per-provider retry/backoff/cooldown
// circuit breaker across LLMs — not just try-next-on-error — matching
// kael-platform's callLLM behavior.
//
// Not used by the Sports Agent pilot: Sports agent's LLM providers are
// kael-platform's llm.LLM, not domain.LLM, and Sports agent's tools/
// workflow run through kael-platform's own, already-production-proven
// nativeLoop via the translation in Kael/agents/domainbridge — not through
// this type. Kept for domain/'s own test suite and for a future phase
// where a domain-native loop is actually wired to a live Agent.
type NativeLoop struct {
	LLMs          []LLM
	MaxIterations int

	mu        sync.Mutex
	downUntil map[int]time.Time
}

func NewNativeLoop(llms []LLM, maxIterations int) *NativeLoop {
	if maxIterations <= 0 {
		maxIterations = defaultMaxIterations
	}
	return &NativeLoop{
		LLMs:          llms,
		MaxIterations: maxIterations,
		downUntil:     make(map[int]time.Time),
	}
}

func (l *NativeLoop) Run(ctx context.Context, messages []Message, actions []*BoundAction) (*LoopResult, []Message, error) {
	actionsByName := make(map[string]*BoundAction, len(actions))
	specs := make([]ActionSpec, 0, len(actions))
	for _, a := range actions {
		actionsByName[a.Spec.Name] = a
		specs = append(specs, a.Spec)
	}

	calledBefore := make(map[string]bool)
	callCounts := make(map[string]int)
	consecutiveFailures := 0

	giveUp := func(reason string) (*LoopResult, []Message, error) {
		return &LoopResult{Status: LoopStatusError, Content: reason}, messages, nil
	}

	for i := 0; l.MaxIterations == 0 || i < l.MaxIterations; i++ {
		resp, err := l.callLLM(ctx, messages, specs)
		if err != nil {
			return nil, messages, fmt.Errorf("native loop: call llm: %w", err)
		}

		if len(resp.ToolCalls) == 0 {
			messages = append(messages,
				Message{Role: RoleAssistant, Content: resp.Content},
				Message{Role: RoleUser, Content: fmt.Sprintf("Respond by calling one of the available actions, or call %q if you're done.", FinishActionName)},
			)
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutiveFailures {
				return giveUp("too many responses with no action call")
			}
			continue
		}

		messages = append(messages, Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls})

		for _, call := range resp.ToolCalls {
			if call.Name == FinishActionName {
				result := &LoopResult{Status: LoopStatusComplete, Output: call.Arguments}
				if content, ok := call.Arguments["content"].(string); ok {
					result.Content = content
				}
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "done"})
				return result, messages, nil
			}

			action, ok := actionsByName[call.Name]
			if !ok {
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: fmt.Sprintf("unknown action %q", call.Name)})
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return giveUp("too many failed action calls")
				}
				continue
			}

			callCounts[call.Name]++
			if callCounts[call.Name] > maxSameActionCalls {
				return giveUp(fmt.Sprintf("action %q called too many times", call.Name))
			}

			callKey := call.Name + "|" + fmt.Sprint(call.Arguments)
			if calledBefore[callKey] {
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "duplicate call with identical arguments, already handled"})
				continue
			}

			output, err := action.Invoke(ctx, call.Arguments)
			if err != nil {
				messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: fmt.Sprintf("error: %v", err)})
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return giveUp("too many failed action calls")
				}
				continue
			}

			calledBefore[callKey] = true
			consecutiveFailures = 0
			messages = append(messages, Message{Role: RoleTool, ToolCallID: call.ID, Name: call.Name, Content: stringifyResult(output)})
		}
	}

	return &LoopResult{Status: LoopStatusMaxIteration}, messages, nil
}

// callLLM tries each provider in priority order. A provider that exhausts
// its retries is marked down for a cooldown window and skipped by later
// calls — except the last remaining provider, which is always tried since
// there's nothing further to fall back to. Mirrors kael-platform's callLLM
// circuit breaker.
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

func stringifyResult(output any) string {
	if s, ok := output.(string); ok {
		return s
	}
	if b, err := json.Marshal(output); err == nil {
		return string(b)
	}
	return fmt.Sprint(output)
}
