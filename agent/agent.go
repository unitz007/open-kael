package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unitz007/kael/human"
	"github.com/unitz007/kael/identity"
	"github.com/unitz007/kael/llm"
	"github.com/unitz007/kael/memory"
	"github.com/unitz007/kael/messaging"
	"github.com/unitz007/kael/rag"
	"github.com/unitz007/kael/tools"
	"github.com/unitz007/kael/triggers"
	"github.com/unitz007/kael/webhook"
	"github.com/unitz007/kael/workflow"

	"github.com/go-co-op/gocron/v2"
	"github.com/robfig/cron/v3"
)

type LLMStatus string
type ToolName string

const (
	LLMStatusComplete     LLMStatus = "completed"
	LLMStatusMaxIteration LLMStatus = "max_iteration_reached"
	LLMToolCallError      LLMStatus = "llm_tool_call_error"
)

type Agent struct {
	Id             string               `json:"id"`
	Name           string               `json:"name"`
	Description    string               `json:"description"`
	OwnerPrompt    string               `json:"owner_prompt"`
	IdentityPrompt string               `json:"identity_prompt"`
	Workflows      []*workflow.Workflow `json:"workflow"`
	Model          string               `json:"model"`
	// LLMs is the ordered list of providers this agent can use. LLMs[0] is
	// the default — every call tries it first. Anything after that is a
	// fallback, tried in order only once the ones before it are judged down
	// (see callLLM) — never round-robined or load-balanced, purely a
	// priority chain. A single-provider agent just has a length-1 slice and
	// behaves exactly as before.
	LLMs []llm.LLM
	// llmState is a parallel slice to LLMs (llmState[i] tracks LLMs[i]) —
	// see callLLM's circuit-breaker logic. Guarded by llmMu since RunLoop
	// can be called concurrently for different conversations.
	llmMu      sync.Mutex
	llmState   []llmProviderState
	Tools      []*tools.ToolSpec `json:"tools"`
	eventBus   EventPublisher
	memory     memory.Memory
	inBox      *MessageQueue
	human      human.Human
	messengers map[string]messaging.Messenger
	// primaryMessenger is whichever Messenger AddMessenger sees first —
	// used by DefaultConversation, so a proactive sending with no active
	// conversation to inherit (a cron/webhook-triggered workflow) has a
	// deterministic platform to land on once an agent has more than one
	// messenger registered. Ranging over messengers itself for this would
	// pick effectively at random: Go map iteration order is randomized,
	// not insertion order.
	primaryMessenger      messaging.Messenger
	directory             AgentDirectory
	webhookMux            *http.ServeMux
	identities            map[string]identity.Identity
	retrievers            map[string]rag.Retriever
	retrieverDescriptions map[string]string
	memoryKeyFunc         messaging.MemoryKeyFunc
	// MaxIterations caps RunLoop and runDelegatedTask's own loops — a
	// workflow's nested run can still go further via its own Iteration
	// field (falling back to this value when unset), but a plain
	// conversation or a plain delegated call has no workflow to carry an
	// override, so this is the only knob for those. Defaults to
	// defaultMaxIterations in NewAgent; set directly to change it.
	//
	// 0 (or negative) means unlimited — the loop then only ends via
	// end_loop or an LLM call error, never by exhausting a count. Setting
	// this removes a real backstop: there's nothing else that forces a
	// stuck or looping model to stop. Only worth doing for an agent you
	// trust to actually call end_loop reliably.
	MaxIterations int

	// MaxDuplicateToolCallsPerResponse is the threshold at which a response
	// containing repeated (identical name+arguments) tool calls gets
	// treated as a decoding glitch — some models get stuck repeating the
	// exact same output, producing dozens to hundreds of identical calls in
	// one shot. A response this size where every call is actually distinct
	// isn't this pattern at all and is let through instead — see
	// MaxToolCallsPerResponse. Defaults to
	// defaultMaxDuplicateToolCallsPerResponse in NewAgent; set directly to
	// change it.
	MaxDuplicateToolCallsPerResponse int

	// MaxToolCallsPerResponse is the hard ceiling on tool calls per
	// response, regardless of whether they're distinct — a circuit breaker
	// against a truly pathological response, not a realistic turn size. A
	// large batch of genuinely distinct calls (e.g. one list_issues call
	// per repo in a repo-by-repo triage workflow) is legitimate multi-tool
	// intent and is let through up to this point, unlike
	// MaxDuplicateToolCallsPerResponse's much lower bar for a response
	// dominated by repeats. Raise this if your own tools/workflows
	// legitimately need bigger batches (more repos, more items, whatever
	// your domain's fan-out looks like); lower it for a stricter safety
	// margin. Defaults to defaultMaxToolCallsPerResponse in NewAgent; set
	// directly to change it.
	MaxToolCallsPerResponse int
}

// AgentDirectory lets an agent see its siblings under a shared host, so it
// can decide what's worth delegating instead of guessing. Defined here
// rather than imported, since the natural implementation (Runtime) already
// imports this package — *runtime.Runtime satisfies this structurally via
// its existing GetAgents method, same trick as EventPublisher below.
type AgentDirectory interface {
	GetAgents() []*Agent
}

// defaultMaxIterations seeds Agent.MaxIterations in NewAgent.
const defaultMaxIterations = 10

// defaultMaxDuplicateToolCallsPerResponse seeds Agent.MaxDuplicateToolCallsPerResponse in NewAgent.
const defaultMaxDuplicateToolCallsPerResponse = 10

// defaultMaxToolCallsPerResponse seeds Agent.MaxToolCallsPerResponse in NewAgent.
const defaultMaxToolCallsPerResponse = 50

// maxConsecutiveToolFailures bounds how many turns in a row can pass
// without a single genuinely new, successful tool call before runLoopFrom
// gives up early — distinct from maxIter, which only bounds the total turn
// count and can't tell a healthy long-running task from a model stuck
// retrying (or repeatedly re-attempting an already-blocked duplicate of)
// the same failing call. Confirmed live in a deployment of this library: an
// agent retried a single blocked tool call every ~2s for minutes straight,
// completely unresponsive to the "you already called it, call end_loop
// now" message each rejection carried — nothing short of a caller's own
// (possibly very high) MaxIterations would have stopped it without this.
const maxConsecutiveToolFailures = 3

// maxSameToolCallsPerRun bounds how many times a single tool name can be
// called in one runLoopFrom run, regardless of whether each call's arguments
// differ — distinct from maxConsecutiveToolFailures, which only fires on
// no-progress turns. A model can call the same tool with genuinely different
// arguments every single time (so every call succeeds, resetting
// consecutiveFailures) while still never making real progress toward
// end_loop — e.g. searching a document one query term at a time instead of
// reading it once. Confirmed live: Personal Assistant Agent called
// search_in_google_doc 48 times over ~3 minutes, all but the last few
// succeeding, before finally repeating itself enough to trip the
// consecutive-failure breaker by accident. This catches that pattern
// directly instead of relying on it eventually degenerating into repeats.
const maxSameToolCallsPerRun = 15

// llmMaxRetries is how many times callLLM retries a single provider (with
// exponential backoff) before treating it as down and falling through to
// the next one in Agent.LLMs — enough to ride out a one-off blip without
// mistaking it for an outage.
const llmMaxRetries = 3

// llmBaseBackoff is the first retry's wait; each subsequent retry within
// the same callLLM attempt doubles it (1s, 2s, 4s for the default
// llmMaxRetries of 3).
const llmBaseBackoff = time.Second

// llmCooldown is how long a provider that just exhausted llmMaxRetries is
// skipped on subsequent calls, once callLLM has already fallen through to
// a later provider for it once — avoids paying the same retry-then-fall-
// through latency on every single call while a provider is genuinely down.
const llmCooldown = 60 * time.Second

// llmProviderState is callLLM's circuit-breaker bookkeeping for one entry
// in Agent.LLMs — see Agent.llmState's own doc comment.
type llmProviderState struct {
	downUntil time.Time // zero value means never tripped / currently healthy
}

// callLLM walks Agent.LLMs in priority order (index 0 first) so every
// runLoopFrom call site gets automatic fallback for free, without needing
// to know how many providers are configured. For each provider it isn't
// currently skipping (see the cooldown check below), it retries up to
// llmMaxRetries times with exponential backoff before moving on — that's
// the "one-off blip vs. a real outage" distinction. A provider that
// exhausts its retries is marked down for llmCooldown so later calls skip
// straight past it instead of re-paying the same retry cost, EXCEPT the
// last provider in the list, which is always tried regardless of cooldown
// — there's nothing left to fall back to, so a skipped attempt there would
// just mean returning an error without ever actually trying.
func (a *Agent) callLLM(messages []llm.Message, toolset []*tools.ToolSpec) (*llm.Response, error) {
	a.llmMu.Lock()
	if len(a.llmState) != len(a.LLMs) {
		a.llmState = make([]llmProviderState, len(a.LLMs))
	}
	a.llmMu.Unlock()

	now := time.Now()
	var lastErr error
	for i, provider := range a.LLMs {
		isLast := i == len(a.LLMs)-1

		a.llmMu.Lock()
		downUntil := a.llmState[i].downUntil
		a.llmMu.Unlock()
		if !isLast && now.Before(downUntil) {
			continue
		}

		var resp *llm.Response
		var err error
		for attempt := 0; attempt <= llmMaxRetries; attempt++ {
			if attempt > 0 {
				time.Sleep(llmBaseBackoff * time.Duration(1<<(attempt-1)))
			}
			resp, err = provider.Call(messages, toolset)
			if err == nil {
				a.llmMu.Lock()
				a.llmState[i].downUntil = time.Time{}
				a.llmMu.Unlock()
				return resp, nil
			}
		}

		lastErr = err
		if len(a.LLMs) > 1 {
			log.Printf("%s: LLM provider %d/%d failed %d times in a row (%v) — %s", a.Name, i+1, len(a.LLMs), llmMaxRetries+1, err,
				map[bool]string{true: "no more providers to fall back to", false: "falling through to the next provider"}[isLast])
		}
		a.llmMu.Lock()
		a.llmState[i].downUntil = now.Add(llmCooldown)
		a.llmMu.Unlock()
	}

	return nil, fmt.Errorf("all %d LLM provider(s) failed, last error: %w", len(a.LLMs), lastErr)
}

// hasDuplicateToolCalls reports whether any two calls in calls share the
// same name and arguments — the signature of a model stuck repeating
// itself, as opposed to a large-but-legitimate batch of genuinely distinct
// calls.
func hasDuplicateToolCalls(calls []tools.ToolCall) bool {
	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		key := c.Name + "\x00" + string(c.Arguments)
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}

// normalizeToolArgs defends against a model calling a tool with no
// arguments — always possible when every parameter is optional. Handlers
// expect the OpenAI-compatible wire shape (a JSON-encoded string wrapping
// the real arguments object) and parse it in two steps; some providers
// send an empty string (or omit the field) instead of "{}" for a no-arg
// call, which crashes that second parse with "unexpected end of JSON
// input" — with nothing actionable for the model to self-correct on, so it
// just retries the identical call forever. Fixed once here, at the single
// point every tool call is dispatched from (runLoopFrom below), rather
// than duplicating the same guard in every handler that decodes args.
func normalizeToolArgs(args json.RawMessage) json.RawMessage {
	var inner string
	if err := json.Unmarshal(args, &inner); err != nil || inner == "" {
		return json.RawMessage(`"{}"`)
	}
	return args
}

// runLoopFrom is the shared tool-calling engine behind every entry point
// (RunLoop, a workflow's own nested run, and a delegated call) — same
// end_loop protocol, same repeat-call guard, just fed a different starting
// transcript, a different tool set, and (since a workflow can legitimately
// need more or fewer steps than a plain conversation) a different iteration
// cap. It returns the final transcript too, so a caller that persists
// conversation history (RunLoop) can see exactly what turns were added. ctx
// flows to every tool call unchanged — this is how a conversation attached
// via messaging.WithConversation reaches send_message's handler.
func (a *Agent) runLoopFrom(ctx context.Context, messages []llm.Message, toolset []*tools.ToolSpec, maxIter, maxDuplicateToolCalls, maxToolCalls, maxSameToolCalls int) (*LoopResult, []llm.Message, error) {
	// Every tool call in this run — agent-level or nested inside a workflow
	// (workflows route through this same function) — can reach whichever of
	// this agent's identities it needs via identity.FromContext, regardless
	// of which agent originally built the tool. Overwriting ctx here is
	// exactly what makes a delegated call resolve against the *target*
	// agent's own identities rather than the delegator's, once
	// runDelegatedTask calls back into this function on the target.
	ctx = identity.WithIdentities(ctx, a.identities)
	ctx = rag.WithRetrievers(ctx, a.retrievers)

	toolsByName := make(map[string]*tools.ToolSpec, len(toolset))
	for _, t := range toolset {
		toolsByName[t.Name] = t
	}

	// Guards against a model that, once every turn must call a tool, defaults
	// to repeating an already-succeeded call (e.g. sending the same message
	// several times) instead of moving on to end_loop.
	calledBefore := make(map[string]bool)

	// toolCallCounts tracks how many separate turns (LLM round-trips) have
	// included at least one attempt at each tool name — success, blocked
	// duplicate, or handler error alike (see the increment site below for
	// why blocked attempts count too) — see maxSameToolCallsPerRun for why
	// this exists separately from calledBefore/consecutiveFailures.
	// Deliberately counts turns, not raw calls: a single turn can
	// legitimately contain many distinct calls to the same tool (e.g. one
	// replace_in_google_doc per fix when applying 24 real, distinct edits
	// at once — confirmed live, see maxSameToolCallsPerRun) and that's real
	// planned work, not the call-one-at-a-time-forever pattern this guards
	// against. seenThisTurn is reset at the top of every outer iteration
	// and dedupes within it.
	toolCallCounts := make(map[string]int)

	// consecutiveFailures counts turns in a row with zero genuinely new,
	// successful tool calls — an unknown tool, a blocked repeat, a real
	// tool.Handler error, a tool-less response, or a discarded duplicate/
	// oversized batch all count; any real success resets it to 0. See
	// maxConsecutiveToolFailures's own doc comment for why this exists
	// alongside, not instead of, maxIter.
	consecutiveFailures := 0

	// giveUp reports (as a non-nil LoopResult) once consecutiveFailures
	// crosses the line — called from every no-progress continue site below,
	// not just the ones inside the tool-execution loop. The three earlier
	// branches (no tool call, discarded duplicate batch, discarded oversized
	// batch) continue the outer loop directly, which would otherwise skip
	// straight past a check placed only after the tool-execution loop. Takes
	// the current iteration explicitly since it's defined before the loop
	// that declares it.
	giveUp := func(iteration int) *LoopResult {
		if consecutiveFailures < maxConsecutiveToolFailures {
			return nil
		}
		log.Printf("⚠️%s made no successful tool call in %d consecutive turns — giving up rather than burning the rest of the %d-iteration budget on repeats", a.Name, consecutiveFailures, maxIter)
		return &LoopResult{Iteration: iteration + 1, Status: LLMToolCallError}
	}

	// maxIter <= 0 means unlimited — the loop then only ever ends via
	// end_loop or an LLM.Call error, never by exhausting a count. There's
	// no other backstop: LLM.Call takes no ctx, so cancelling the caller's
	// context doesn't interrupt an in-flight or subsequent call either.
	// The repeat-call guard above and the runaway-tool-call-batch check
	// below still apply exactly as they do with a normal cap — this only
	// removes the iteration count as a termination condition.
	for i := 0; maxIter <= 0 || i < maxIter; i++ {
		// seenThisTurn dedupes toolCallCounts within a single turn — see
		// toolCallCounts's own doc comment.
		seenThisTurn := make(map[string]bool)

		response, err := a.callLLM(messages, toolset)
		if err != nil {
			log.Println("Error:", err)
			return nil, messages, err
		}

		if len(response.ToolCalls) == 0 {
			// Shouldn't happen with tool_choice=required, but a provider can
			// still ignore it. Silently accepting that content as the final
			// answer is exactly how an ungrounded response (invented rather
			// than fetched via a real tool, e.g. a movie recommendation that
			// skipped get_now_playing) would sail straight through — the
			// content was never verified against anything. Nudge back onto
			// protocol instead: force either a real tool call or an explicit
			// end_loop, consuming an iteration rather than trusting freeform
			// text as if it were a legitimate, tool-checked answer. A
			// provider that truly never honors tool_choice still terminates
			// via the maxIter cap below — loudly, as LLMStatusMaxIteration,
			// not silently as an unverified LLMStatusComplete.
			log.Printf("⚠️%s returned plain text instead of a tool call on iteration %d — nudging back onto protocol instead of accepting it as a final answer. Content was: %q, finish_reason: %q, reasoning: %q", a.Name, i+1, response.Content, response.FinishReason, response.Reasoning)
			messages = append(messages,
				llm.Message{Role: "assistant", Content: response.Content},
				llm.Message{Role: "user", Content: "That response had no tool call, so it wasn't recorded as an answer. If you already have everything you need, call end_loop with your final_message now. Otherwise call the tool you actually need next — don't state an answer without the tool call that backs it up."},
			)
			consecutiveFailures++
			if r := giveUp(i); r != nil {
				return r, messages, nil
			}
			continue
		}

		if len(response.ToolCalls) > maxDuplicateToolCalls && hasDuplicateToolCalls(response.ToolCalls) {
			// A response this size dominated by repeated calls — some models
			// get stuck repeating the exact same output — isn't legitimate
			// multi-tool use. Left unchecked, processing all of them floods
			// the log with "blocked" repeats (only the first of any given
			// call actually runs) and bloats the transcript pointlessly.
			// Discard the whole batch without executing or recording any of
			// it, and nudge for a single deliberate call instead — same
			// "consume an iteration, try again" shape as the tool-less-
			// response case above. A same-size response where every call is
			// actually distinct (e.g. one list_issues call per installed
			// repo) skips this entirely — see MaxToolCallsPerResponse below.
			log.Printf("⚠️%s's response contained %d tool calls in one turn with repeats (cap %d) — discarding as a decoding glitch rather than executing any of them", a.Name, len(response.ToolCalls), maxDuplicateToolCalls)
			messages = append(messages,
				llm.Message{Role: "assistant", Content: response.Content},
				llm.Message{Role: "user", Content: "That response repeated tool calls an unreasonable number of times in one turn, so none of them were executed. Try again with one single, deliberate tool call."},
			)
			consecutiveFailures++
			if r := giveUp(i); r != nil {
				return r, messages, nil
			}
			continue
		}

		if len(response.ToolCalls) > maxToolCalls {
			// Every call here is distinct (the repeat case above already
			// returned), so this is a genuinely large batch, not a glitch —
			// still capped as a last-resort circuit breaker against a truly
			// pathological response.
			log.Printf("⚠️%s's response contained %d distinct tool calls in one turn (cap %d) — discarding rather than executing all of them", a.Name, len(response.ToolCalls), maxToolCalls)
			messages = append(messages,
				llm.Message{Role: "assistant", Content: response.Content},
				llm.Message{Role: "user", Content: "That response tried to call an unreasonable number of distinct tools in one turn, so none of them were executed. Split this into smaller batches across multiple turns."},
			)
			consecutiveFailures++
			if r := giveUp(i); r != nil {
				return r, messages, nil
			}
			continue
		}

		messages = append(messages, llm.Message{
			Role:      "assistant",
			ToolCalls: response.ToolCalls,
			Content:   response.Content,
		})

		for _, toolCall := range response.ToolCalls {
			tool, ok := toolsByName[toolCall.Name]
			if !ok {
				log.Printf("Tool %s not found in agent's tools", toolCall.Name)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    fmt.Sprintf("error: unknown tool %q", toolCall.Name),
					ToolCallID: toolCall.Id,
				})
				consecutiveFailures++
				continue
			}

			// Counts every attempt at this tool this turn — success, blocked
			// duplicate, or handler error alike — not just successes. A model
			// that alternates between two tools (one succeeding with varying
			// arguments, the other repeatedly blocked as an exact duplicate)
			// never hits 3 consecutive failures, since the successful calls
			// keep resetting that counter — confirmed live, a run alternated
			// query_conversation_history (succeeding) with get_thread_history
			// (blocked every time) for 20+ turns before this existed, caught
			// by neither guard. Counting attempts here closes that gap: the
			// repeatedly-blocked tool now accumulates toward its own cap
			// exactly like a repeatedly-varying one does.
			if toolCall.Name != "end_loop" && !seenThisTurn[toolCall.Name] {
				seenThisTurn[toolCall.Name] = true
				toolCallCounts[toolCall.Name]++
				if toolCallCounts[toolCall.Name] > maxSameToolCalls {
					log.Printf("⚠️%s attempted %s across %d separate turns in this run — giving up rather than continuing what looks like an unproductive one-call-at-a-time pattern", a.Name, toolCall.Name, toolCallCounts[toolCall.Name])
					return &LoopResult{Iteration: i + 1, Status: LLMToolCallError}, messages, nil
				}
			}

			callKey := toolCall.Name + "\x00" + string(toolCall.Arguments)
			if calledBefore[callKey] && toolCall.Name != "end_loop" {
				log.Printf("⚠️%s tried to repeat %s — blocked", a.Name, toolCall.Name)
				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: toolCall.Id,
					Name:       tool.Name,
					Content:    fmt.Sprintf("error: %s was not executed again — you already called it with these exact arguments and it succeeded. If you're finished, call end_loop now with your final_message.", toolCall.Name),
				})
				consecutiveFailures++
				continue
			}

			result, err := a.callTool(ctx, tool, normalizeToolArgs(toolCall.Arguments))
			if err != nil {
				log.Println("Tool execution error:", err.Error())
				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: toolCall.Id,
					Name:       tool.Name,
					Content:    fmt.Sprintf("error: %v", err),
				})
				consecutiveFailures++
				continue
			}
			calledBefore[callKey] = true
			consecutiveFailures = 0

			if toolCall.Name == "end_loop" {
				er, ok := result.(endLoopResult)
				if !ok {
					log.Printf("⚠️end_loop handler returned unexpected type %T", result)
				}
				//log.Println("end_loop reason:", er.Reason)
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    "Loop ended by tool call",
					ToolCallID: toolCall.Id,
					Name:       tool.Name,
				})
				return &LoopResult{Iteration: i + 1, Status: LLMStatusComplete, Content: er.FinalMessage}, messages, nil
			}

			// Deliberately just the agent and tool name, not result — the
			// point is diagnosability (which agent, which tool, was this
			// what looped) not a full audit trail, and several of these
			// tools' results carry real financial/personal data that has
			// no reason to also live in Fly's log stream. This was
			// commented out before; restoring it is exactly what would
			// have made three separate live incidents this session
			// (stuck-in-a-loop agents with no way to tell what they were
			// calling) diagnosable in the moment instead of after the fact.
			log.Printf("🔧%s: %s succeeded", a.Name, tool.Name)

			var content string
			switch v := result.(type) {
			case string:
				content = v
			default:
				if b, mErr := json.Marshal(v); mErr == nil {
					content = string(b)
				} else {
					content = fmt.Sprintf("%v", v)
				}
			}

			messages = append(messages, llm.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: toolCall.Id,
				Name:       toolCall.Name,
			})
		}

		if r := giveUp(i); r != nil {
			return r, messages, nil
		}
	}

	log.Printf("⏱️%s hit the %d-iteration cap without calling end_loop", a.Name, maxIter)
	return &LoopResult{Iteration: maxIter, Status: LLMStatusMaxIteration}, messages, nil
}

func (a *Agent) AddWorkflow(workflow *workflow.Workflow) {
	a.Workflows = append(a.Workflows, workflow)
}

func (a *Agent) AddTool(tool *tools.ToolSpec) {
	a.Tools = append(a.Tools, tool)
}

// SetMemory sets this agent's conversation memory. NewAgent's own default
// is a bare, process-local, in-memory implementation (this library ships no
// persistent Memory implementation — see examples/starter for a
// file-backed reference to copy, or bring a database-backed one of your
// own). Call before Start (or before the first RunLoop) — a change mid
// conversation would silently orphan whatever was already recorded in the
// old store.
func (a *Agent) SetMemory(m memory.Memory) {
	a.memory = m
}

// SetEventBus wires this agent's lifecycle/workflow events into an external
// publisher (e.g. a Runtime's shared events.EventBus). Optional — a nil
// eventBus (the default) just means those PublishEvent calls are skipped.
func (a *Agent) SetEventBus(eventBus EventPublisher) {
	a.eventBus = eventBus
}

// SetDirectory wires this agent into a shared registry of sibling agents
// (e.g. a Runtime), so delegateToolSpecs has agents to expose as tools.
// Optional — a nil directory (the default) just means no delegate tools
// show up, same as an agent with no workflows gets no workflow tools.
func (a *Agent) SetDirectory(d AgentDirectory) {
	a.directory = d
}

// SetWebhookMux wires the shared HTTP mux (e.g. a Runtime's) that Start
// registers this agent's webhook-triggered workflows onto.
func (a *Agent) SetWebhookMux(mux *http.ServeMux) {
	a.webhookMux = mux
}

// AddMessenger registers a platform adapter (Telegram, Slack, ...) this
// agent can send through and listen on. Register before calling Start —
// Start is what actually launches each registered messenger's Listen loop.
// send_message (built into every agent, see NewAgent) routes to whichever
// messenger matches the active conversation's Platform.
func (a *Agent) AddMessenger(m messaging.Messenger) {
	if a.messengers == nil {
		a.messengers = make(map[string]messaging.Messenger)
	}
	a.messengers[m.Platform()] = m
	if a.primaryMessenger == nil {
		a.primaryMessenger = m
	}
}

// IdentifyAs declares who this agent is on an external system — a specific
// GitHub App installation, a specific AWS role, etc. Keyed by Provider(),
// so an agent can hold one declared identity per external system. Tools
// don't take an Identity directly (a workflow's tools are built before the
// owning agent even exists, so there's nothing to close over yet) — they
// resolve whichever identity they need at call time via
// identity.FromContext, since runLoopFrom threads a.identities into ctx on
// every run.
func (a *Agent) IdentifyAs(id identity.Identity) {
	if a.identities == nil {
		a.identities = make(map[string]identity.Identity)
	}
	a.identities[id.Provider()] = id
}

// AddRetriever registers a queryable, already-indexed source (a codebase, a
// docs corpus, ...) under name — an agent can hold more than one, e.g.
// "codebase" and "docs". description is shown to the model as the reason to
// call this source's generated tool (see retrieverToolSpecs) — write it the
// way you'd write any other ToolSpec description, since that's exactly
// where it ends up. Same reasoning as IdentifyAs: a tool doesn't take a
// Retriever directly, since a workflow's tools are built before the owning
// agent even exists — they resolve whichever retriever they need at call
// time via rag.FromContext, since runLoopFrom threads a.retrievers into ctx
// on every run.
func (a *Agent) AddRetriever(name, description string, r rag.Retriever) {
	if a.retrievers == nil {
		a.retrievers = make(map[string]rag.Retriever)
		a.retrieverDescriptions = make(map[string]string)
	}
	a.retrievers[name] = r
	a.retrieverDescriptions[name] = description
}

// EnqueueMessage is the external entry point for feeding a message to this
// agent — main.go, a Messenger's Listen callback, or a test. Safe to call
// before Start(): Enqueue just buffers on the channel regardless of whether
// a listener is attached yet. InBox/MessageQueue stay internal; callers
// never need to touch them directly.
func (a *Agent) EnqueueMessage(conv messaging.ConversationRef, text, messageID, threadID, workflowID string) {
	a.inBox.Enqueue(InBox{Conversation: conv, Payload: text, MessageID: messageID, ThreadID: threadID, WorkflowID: workflowID})
}

// endLoopResult carries the two things end_loop captures: a bookkeeping
// note (Reason) and the actual answer meant for the user (FinalMessage).
// Once tool_choice is forced, plain-content-only responses are no longer
// possible, so FinalMessage is the only place the model's answer lands.
type endLoopResult struct {
	Reason       string
	FinalMessage string
}

// loopProtocolInstructions explains the end_loop/tool_choice protocol every
// runLoopFrom call is subject to — shared between an agent's own
// IdentityPrompt and a workflow's SystemPrompt (which predates this
// protocol and doesn't mention it) when a workflow runs as a nested loop.
//
// The diminishing-returns sentence was added after a live incident: an
// agent trying to recall a specific past answer called the same kind of
// tool 15+ times in a row, each with different arguments, never converging
// — every individual call succeeded, so nothing here told it to stop, and
// it ran until a separate mechanical cap (MaxSameToolCallsPerRun) cut it
// off. That cap is still the real backstop (this is a prose instruction, not
// a guarantee), but nothing was telling the model itself when to give up on
// a line of exploration — deliberately worded around "repeating a similar
// action," not any specific tool, so it generalizes to whatever the next
// version of this pattern looks like, on any agent.
const loopProtocolInstructions = "You have access to tools. " +
	"When you have fully completed the request and have nothing further to do, call the end_loop tool. " +
	"Do not call end_loop while you still have pending work. " +
	"Put your actual answer in end_loop's final_message argument — every response must call a tool, so that is the only place your answer is captured. " +
	"Never repeat a tool call with the same arguments after it has already succeeded — one send is enough. As soon as an action succeeds, call end_loop immediately unless explicitly asked for it to happen more than once." +
	"Never expose a tool name or argument in your final answer — the user should not see any tool calls, only the final_message you put in end_loop." +
	"If repeating a similar action with different inputs stops turning up anything new, that's information too — don't keep trying variations hoping for a better result. Work with what you've already found, or if that's not enough to proceed confidently, say so in end_loop's final_message and ask what you need, rather than continuing indefinitely."

// NewAgent builds an Agent bound to one or more LLM providers. llms[0] is
// the default, tried first on every call — anything after it is a
// fallback, used only once earlier providers are judged down (see
// Agent.callLLM). Passing a single provider (the common case) behaves
// exactly as before.
func NewAgent(id, name, description, identityPrompt string, llms ...llm.LLM) *Agent {
	if len(llms) == 0 {
		panic("agent: NewAgent requires at least one LLM provider")
	}
	a := &Agent{
		Id:             id,
		Name:           name,
		Description:    description,
		IdentityPrompt: identityPrompt + loopProtocolInstructions,
		Workflows:      make([]*workflow.Workflow, 0),
		LLMs:           llms,
		llmState:       make([]llmProviderState, len(llms)),
		Tools: []*tools.ToolSpec{
			tools.NewToolBuilder("end_loop", "Call this when you have fully completed the user's request and have nothing further to do. Do not call this if you still need to call another tool").
				Parameter("final_message", "string", "The actual final answer to the user. This is the one place your answer is captured — do not leave it empty.", true).
				Parameter("reason", "string", "Brief note on why the task is complete.", false).
				Handler(func(ctx context.Context, args json.RawMessage) (any, error) {

					if args == nil {
						return endLoopResult{FinalMessage: "Loop ended (no message provided)."}, nil
					}

					var raw string
					if err := json.Unmarshal(args, &raw); err != nil {
						return nil, err
					}

					var input struct {
						Reason       string `json:"reason"`
						FinalMessage string `json:"final_message"`
					}

					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						return nil, err
					}

					return endLoopResult{Reason: input.Reason, FinalMessage: input.FinalMessage}, nil
				}).Build(),
		},
		eventBus:                         nil,
		memory:                           newBareMemory(),
		inBox:                            NewMessageQueue(32),
		MaxIterations:                    defaultMaxIterations,
		MaxDuplicateToolCallsPerResponse: defaultMaxDuplicateToolCallsPerResponse,
		MaxToolCallsPerResponse:          defaultMaxToolCallsPerResponse,
	}

	return a
}

// bareMemory is NewAgent's zero-config memory.Memory default — process-local,
// lost on restart. This library ships no persistent implementation (same
// reasoning as identity/webhook/rag/messaging — see examples/starter for a
// file-backed reference to copy, or bring your own database-backed one via
// SetMemory).
type bareMemory struct {
	mu   sync.Mutex
	byID map[string][]llm.Message
}

func newBareMemory() *bareMemory {
	return &bareMemory{byID: make(map[string][]llm.Message)}
}

func (m *bareMemory) History(ctx context.Context, id string) []llm.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.byID[id]
	out := make([]llm.Message, len(existing))
	copy(out, existing)
	return out
}

func (m *bareMemory) Append(ctx context.Context, id string, messages ...llm.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	updated := append(m.byID[id], messages...)
	if len(updated) > maxBareMemoryMessages {
		updated = updated[len(updated)-maxBareMemoryMessages:]
	}
	m.byID[id] = updated
}

// maxBareMemoryMessages bounds how many turns bareMemory keeps per id — a
// plain trim-the-oldest window, not summarization.
const maxBareMemoryMessages = 20

// baseTools returns this agent's own tools, plus send_message and whatever
// extra tools each registered messenger contributes via messengerTools —
// but only once a messenger is actually registered (AddMessenger). An
// agent with no messenger at all (e.g. ResearchSpecialistAgent) has nowhere
// for send_message to route to, so offering it just invites a
// guaranteed-to-fail call ("no messenger registered for platform ...").
// Computed fresh each call (like workflowToolSpecs/delegateToolSpecs) since
// AddMessenger can be called any time after NewAgent.
//
// Used by both RunLoop's real-conversation path (and a workflow running as
// part of one) and runDelegatedTask — a delegated call routes send_message
// and any messenger tools through whatever ConversationRef it inherited via
// ctx from the delegating call, using its own (the delegate's) registered
// messenger. See messengerTools for the caveat that implies.
func (a *Agent) baseTools() []*tools.ToolSpec {
	return append(a.defaultTools(), a.messagingTools()...)
}

// defaultTools is a.Tools (everything added via AddTool, including
// anything RequiresApproval) plus retrievers and thread-history — every
// tool with no dependency on there being a live, identity-matched
// conversation to act through. Never gated behind anything. See
// Agent.messagingTools for the one category that is gated, and why.
func (a *Agent) defaultTools() []*tools.ToolSpec {
	out := append([]*tools.ToolSpec{}, a.Tools...)
	out = append(out, a.retrieverToolSpecs()...)
	if a.memory != nil {
		out = append(out, a.getThreadHistoryTool())
	}
	return out
}

// messagingTools is send_message plus any messenger-contributed tools
// (add_reaction, ...) — the only tools that require a live,
// identity-matched conversation to act through, and so the only ones
// runWorkflow ever excludes. Everything else a workflow might need
// (a.Tools — including anything RequiresApproval — plus retrievers and
// thread-history) is unconditional; those have no dependency on there
// being a live conversation and were never a meaningful thing to gate.
// See runWorkflow's own doc comment for where that exclusion actually
// happens and why.
func (a *Agent) messagingTools() []*tools.ToolSpec {
	if len(a.messengers) == 0 {
		return nil
	}
	return append([]*tools.ToolSpec{a.sendMessageTool()}, a.messengerTools()...)
}

// retrieverToolSpecs turns every registered rag.Retriever into a callable
// query_<name> tool — the generic bridge nothing in this codebase had
// before: AddRetriever only ever put a Retriever into a.retrievers/ctx, and
// nothing turned that into something the model could actually call. Any
// retriever registered via AddRetriever gets this automatically, the same
// inheritance workflowToolSpecs/messengerTools already give their own
// sources.
func (a *Agent) retrieverToolSpecs() []*tools.ToolSpec {
	specs := make([]*tools.ToolSpec, 0, len(a.retrievers))
	for name, r := range a.retrievers {
		specs = append(specs, a.retrieverToolSpec(name, a.retrieverDescriptions[name], r))
	}
	return specs
}

// defaultRetrieverTopK bounds how many results a query_<name> tool call
// returns — generous enough to be useful, small enough not to flood the
// model's context with marginal matches.
const defaultRetrieverTopK = 5

func (a *Agent) retrieverToolSpec(name, description string, r rag.Retriever) *tools.ToolSpec {
	if description == "" {
		description = fmt.Sprintf("Searches %s for relevant context.", name)
	}
	return tools.NewToolBuilder("query_"+name, description).
		Parameter("query", "string", "What to search for", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			var input struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}
			if input.Query == "" {
				return nil, fmt.Errorf("query_%s: query is required", name)
			}

			results, err := r.Query(ctx, input.Query, defaultRetrieverTopK)
			if err != nil {
				return nil, err
			}
			// Logs the query and result count (not result content — same
			// reasoning the per-tool "🔧 succeeded" log line elsewhere in
			// this file already follows: diagnosability without putting
			// real conversation content, which can carry PII, into Fly's
			// log stream) — added after a live incident where a model
			// guessed at query_conversation_history phrasing 15+ times in a
			// row with zero visibility into what it was actually searching
			// for or getting back at each attempt.
			log.Printf("🔎%s: query_%s(%q) → %d result(s)", a.Name, name, input.Query, len(results))
			if len(results) == 0 {
				// Deliberately doesn't just say "No matches found" — a
				// model getting that back for query after query has no
				// signal to change strategy on, which is exactly what drove
				// the live incident above: 16 different guessed phrasings,
				// each returning this same uninformative string. Naming the
				// actual limitation (most Retriever implementations here
				// are exact/keyword search, not semantic) gives it a real
				// reason to stop guessing and ask instead.
				return "No matches found for that exact query. If this is exact/keyword-based search, a paraphrase can miss content that's genuinely there — try the other party's own literal wording if you know it, and if you don't have a specific term to try, ask the owner directly rather than guessing more phrasings.", nil
			}

			var b strings.Builder
			for i, res := range results {
				fmt.Fprintf(&b, "%d. [id: %s] (score %.2f) %s\n", i+1, res.Document.ID, res.Score, res.Document.Content)
			}
			return b.String(), nil
		}).Build()
}

// deepHistory is an optional capability a memory.Memory implementation can
// satisfy to let getThreadHistoryTool pull a genuinely bigger window than
// History's own default — same optional-interface pattern as
// rag.Retriever/messaging.ToolProvider elsewhere in this codebase.
// Without this, get_thread_history calls straight through to History and
// gets back exactly the same small window already auto-injected into every
// turn — confirmed live: an 8-message default window meant to keep
// per-turn cost down also silently capped the one tool whose entire job is
// to deliberately pull *more* than that, defeating its own purpose. A
// Memory backed by something with no natural "give me N instead" knob
// (e.g. NewAgent's own bareMemory) just falls back to History() below —
// not a functional loss, since that implementation has nothing larger to
// give anyway.
type deepHistory interface {
	HistoryN(ctx context.Context, id string, n int) []llm.Message
}

// threadHistoryWindow bounds a get_thread_history pull on a Memory that
// implements deepHistory — deliberately much larger than the default
// auto-injected window (see deepHistory's own doc comment): this only runs
// when the model explicitly decided it needs deep context, so the extra
// cost is justified, unlike paying it on every single turn by default.
const threadHistoryWindow = 40

// getThreadHistoryTool returns the full transcript for a specific id — the
// same string a query_<name> retriever tool's result Document.ID/Source
// returned, otherwise not something the model has any way to discover on
// its own. Only needs the base Memory interface, not Searchable/Retriever,
// but is only really useful alongside a retriever tool that can surface an
// id to begin with — see baseTools, which folds this in whenever a.memory
// is set at all.
func (a *Agent) getThreadHistoryTool() *tools.ToolSpec {
	return tools.NewToolBuilder("get_thread_history", "Fetches the full message history for a specific id returned by a query_* search tool, for full context around a match.").
		Parameter("thread_id", "string", "The id value from a query_* search result", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			var input struct {
				ThreadID string `json:"thread_id"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}
			if input.ThreadID == "" {
				return nil, fmt.Errorf("get_thread_history: thread_id is required")
			}

			var messages []llm.Message
			if dh, ok := a.memory.(deepHistory); ok {
				messages = dh.HistoryN(ctx, input.ThreadID, threadHistoryWindow)
			} else {
				messages = a.memory.History(ctx, input.ThreadID)
			}
			if len(messages) == 0 {
				return "No history found for that thread_id.", nil
			}
			var b strings.Builder
			for _, msg := range messages {
				fmt.Fprintf(&b, "[%s] %s\n", msg.Role, msg.Content)
			}
			return b.String(), nil
		}).Build()
}

// messengerTools collects whatever extra tools each registered messenger
// contributes via messaging.ToolProvider (e.g. Slack's
// add_reaction/search_emoji) — folded into baseTools, and inherited
// automatically here rather than needing each agent's constructor to
// AddTool them by hand, the same reasoning send_message itself already
// follows, just generalized to any Messenger implementation that has more
// to offer than Send/Listen.
//
// Note: a delegated agent's own registered messenger is what actually
// performs the reaction or send (e.g. its own Slack bot identity), even
// though the ConversationRef/MessageID came from the delegating agent's
// inbound message (inherited via ctx) — if that bot isn't a member of the
// channel the message lives in, the call just fails with a normal tool
// error, same as any other platform-side rejection.
func (a *Agent) messengerTools() []*tools.ToolSpec {
	var out []*tools.ToolSpec
	for _, m := range a.messengers {
		if tp, ok := m.(messaging.ToolProvider); ok {
			out = append(out, tp.Tools()...)
		}
	}
	return out
}

// sendMessageTool is built into every agent, same as end_loop — always
// present, works the instant any messenger is registered via AddMessenger.
// No messenger, no workflow, no external constructor owns this anymore;
// routing to the right platform is intrinsically the agent's own job.
func (a *Agent) sendMessageTool() *tools.ToolSpec {
	return tools.NewToolBuilder("send_message", "Sends a message back to the user in the current conversation.").
		Parameter("message", "string", "The message to send", true).
		Parameter("platform", "string", "Optional. Send through a specific registered platform instead of the current conversation — e.g. \"email\" to email the owner instead of replying here. Only use this when the user explicitly asks to be reached a different way (\"send that to my email\"); leave unset for a normal reply. Fails if that platform isn't registered for this agent.", false).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			if args == nil {
				return nil, fmt.Errorf("send_message: no arguments provided")
			}

			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}

			var input struct {
				Message  string `json:"message"`
				Platform string `json:"platform"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			target, messenger, err := a.resolveSendTarget(ctx, input.Platform)
			if err != nil {
				log.Println("send_message: could not resolve a target:", err)
				return nil, err
			}
			log.Printf("📤%s: routing send_message to %s/%s", a.Name, target.Platform, target.ChatID)

			input.Message = consumeDelegationNotes(ctx) + input.Message
			if _, err := a.replyOrSend(ctx, messenger, target, input.Message); err != nil {
				log.Println("Error sending message:", err)
				return nil, err
			}
			return "Message sent successfully", nil
		}).Build()
}

// resolveSendTarget picks which conversation/messenger send_message should
// use. platform, when non-empty, is an explicit redirect request (e.g. the
// user asked to be emailed instead of replied to here) — it's honored only
// if a messenger is actually registered for it, and lands on that
// messenger's own DefaultConversation unless the active conversation from
// ctx already happens to be on that same platform (then the real
// conversation is used instead, preserving thread/channel context rather
// than falling back to a generic default). With no platform requested,
// this is the active conversation from ctx if one's attached (replying to
// an inbound message), or the first registered messenger's own default
// conversation otherwise (a proactive send — e.g. a cron-triggered
// workflow with nothing to reply to).
func (a *Agent) resolveSendTarget(ctx context.Context, platform string) (messaging.ConversationRef, messaging.Messenger, error) {
	if platform != "" {
		m, ok := a.messengers[platform]
		if !ok {
			return messaging.ConversationRef{}, nil, fmt.Errorf("no messenger registered for platform %q", platform)
		}
		if conv, ok := messaging.ConversationFromContext(ctx); ok && conv.Platform == platform {
			return conv, m, nil
		}
		return m.DefaultConversation(), m, nil
	}

	if conv, ok := messaging.ConversationFromContext(ctx); ok {
		m, ok := a.messengers[conv.Platform]
		if !ok {
			return messaging.ConversationRef{}, nil, fmt.Errorf("no messenger registered for platform %q", conv.Platform)
		}
		return conv, m, nil
	}

	if conv, ok := a.DefaultConversation(); ok {
		return conv, a.messengers[conv.Platform], nil
	}
	return messaging.ConversationRef{}, nil, fmt.Errorf("no messenger registered")
}

// callTool runs tool.Handler, first enforcing RequiresApproval if the tool
// declares it (see ToolSpec.RequiresApproval's own doc comment — the tool
// only says approval is needed, never how to obtain it). Deliberately
// resolves the approval target via a.DefaultConversation() — this agent's
// own identity — rather than resolveSendTarget's ambient-ctx-preferring
// logic (what send_message uses): ctx can carry a ConversationRef
// inherited from a completely different agent (e.g. mid-delegation — see
// runDelegatedTask, which forwards its caller's ctx unchanged into its own
// nested loop), and resolveSendTarget would happily pair that foreign
// ConversationRef with THIS agent's own messenger — posting an approval
// prompt as the wrong bot into a channel it likely isn't even a member
// of. An approval prompt is always something this agent originates on its
// own behalf, never a reply within someone else's conversation, so it
// should never inherit one. Requires the resolved messenger to implement
// messaging.ApprovalMessenger; one that doesn't is refused outright rather
// than treated as "no approval needed" — never a silent bypass. A human
// decline or an unanswered prompt both come back as a normal, non-error
// result (Handler never runs) so the model sees "not approved" like any
// other tool outcome, not a failure to retry.
func (a *Agent) callTool(ctx context.Context, tool *tools.ToolSpec, args json.RawMessage) (any, error) {
	if !tool.RequiresApproval {
		return tool.Handler(ctx, args)
	}

	conv, ok := a.DefaultConversation()
	if !ok {
		return nil, fmt.Errorf("%s requires approval but no messenger is available", tool.Name)
	}
	m := a.messengers[conv.Platform]
	am, ok := m.(messaging.ApprovalMessenger)
	if !ok {
		return nil, fmt.Errorf("%s requires approval, but %s doesn't support interactive approval — refusing to run rather than executing unapproved", tool.Name, m.Platform())
	}

	timeout := tool.ApprovalTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	approveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	approved, err := am.RequestApproval(approveCtx, conv, tool.ApprovalSummarize(ctx, args))
	if err != nil {
		return nil, fmt.Errorf("%s approval wait failed: %w", tool.Name, err)
	}
	if !approved {
		return "Not approved within the timeout — action not taken.", nil
	}
	return tool.Handler(ctx, args)
}

// replyOrSend delivers text to conv via m, addressed back to the
// triggering inbound message when ctx actually carries one for that same
// conversation (a genuine reply), or falling back to a plain proactive
// Send otherwise — covers both a cron/webhook-triggered workflow with
// nothing to reply to, and resolveSendTarget having redirected to a
// different platform than the one the message came from (replying "into"
// a conversation on a platform other than where it started doesn't mean
// anything, so that's always a fresh Send too).
func (a *Agent) replyOrSend(ctx context.Context, m messaging.Messenger, conv messaging.ConversationRef, text string) (messaging.MessengerResponse, error) {
	msgID, hasMsg := messaging.MessageIDFromContext(ctx)
	origConv, hasConv := messaging.ConversationFromContext(ctx)
	if hasMsg && hasConv && origConv == conv {
		threadID, _ := messaging.ThreadIDFromContext(ctx)
		return m.Reply(ctx, messaging.InboundMessage{
			Conversation: conv,
			MessageID:    msgID,
			ThreadID:     threadID,
		}, text)
	}
	return m.Send(ctx, conv, text)
}

// DefaultConversation returns the first-registered messenger's default
// conversation — where a proactive send with no active conversation in
// context goes (see primaryMessenger's own doc comment for why this has
// to be insertion-ordered rather than an arbitrary pick across
// messengers). ok is false if no messenger is registered at all. External
// callers (main.go) use this to address an initial message without needing
// to know which platform is actually registered.
func (a *Agent) DefaultConversation() (conv messaging.ConversationRef, ok bool) {
	if a.primaryMessenger == nil {
		return messaging.ConversationRef{}, false
	}
	return a.primaryMessenger.DefaultConversation(), true
}

// SetMemoryKeyFunc overrides how this agent partitions memory across
// RunLoop/runWorkflow/runDelegatedTask — see messaging.MemoryKeyFunc and
// its presets (KeyByAgent, KeyByConversation, KeyByThread, KeyByWorkflow).
// Defaults to messaging.KeyByAgent() (one shared thread for the entire
// agent, regardless of platform/conversation/thread) if never called —
// this agent's original behavior, unchanged for anyone not opting in. That
// default exists for the same reason a single-owner agent (one person,
// reachable through several Messengers — e.g. both Telegram and Slack)
// might want one continuous thread of context no matter which platform
// they use, rather than a fresh, disconnected one every time they switch —
// still available via KeyByAgent, just no longer the only option.
func (a *Agent) SetMemoryKeyFunc(fn messaging.MemoryKeyFunc) {
	a.memoryKeyFunc = fn
}

// SetHuman wires h onto this agent — its Details() is read fresh on every
// prompt build (see humanBlock). Normally called once by Runtime.SetHuman
// on every registered agent, not per agent directly; exposed here so an
// agent used standalone (no Runtime) can still opt in.
func (a *Agent) SetHuman(h human.Human) {
	a.human = h
}

// memoryKey resolves the configured (or default) MemoryKeyFunc for the
// current run.
func (a *Agent) memoryKey(ctx context.Context, conv messaging.ConversationRef) string {
	if a.memoryKeyFunc == nil {
		return messaging.KeyByAgent()(ctx, conv)
	}
	return a.memoryKeyFunc(ctx, conv)
}

type EventPublisher interface {
	PublishEvent(eventType, agentName, workflow, message string, err error, data map[string]interface{})
}

// missedCronLookback bounds how far back warnIfCronRunLikelyMissed searches
// for cronExpr's most recent past occurrence — 8 days comfortably covers
// even a weekly schedule (the least frequent trigger any workflow here
// actually uses) with margin to spare.
const missedCronLookback = 8 * 24 * time.Hour

// recoverFromPanic must be deferred at the top of every goroutine that runs
// agent-supplied logic (LLM calls, tool handlers) outside the shared inbox
// queue (which recovers on its own) — this binary hosts several independent
// agents in one process, so a panic in one agent's cron- or webhook-triggered
// workflow run must not take the other agents down with it.
func recoverFromPanic(agentName, context string) {
	if r := recover(); r != nil {
		log.Printf("⚠️%s: recovered from panic in %s: %v\n%s", agentName, context, r, debug.Stack())
	}
}

// missedCronWindow is how close to "now" that most recent past occurrence
// can be before it's treated as a likely-skipped run.
const missedCronWindow = 15 * time.Minute

// warnIfCronRunLikelyMissed logs a warning when cronExpr's most recent past
// occurrence fell within missedCronWindow of the moment this workflow is
// being (re-)registered. gocron only ever schedules the *next* occurrence
// forward from whenever a process starts — it has no memory of whether a
// run it should have fired actually happened, so a restart (redeploy,
// crash) landing close enough to a trigger time silently drops that run
// with nothing else anywhere logging it. This can't distinguish "genuinely
// skipped" from "fired moments ago under the previous process and this is a
// coincidental restart" — it's a heads-up to go check, not a certainty.
func warnIfCronRunLikelyMissed(agentName, workflowName, cronExpr string) {
	sched, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return // cronExpr's own validity is checked separately, at actual schedule time
	}

	now := time.Now()
	last := now.Add(-missedCronLookback)
	for {
		next := sched.Next(last)
		if next.After(now) {
			break
		}
		last = next
	}

	if gap := now.Sub(last); gap < missedCronWindow {
		log.Printf("⚠️%s: workflow %q's cron schedule (%s) was due at %s, %s before this process started — if that run didn't actually happen, it was likely skipped by a restart landing right on the trigger time", agentName, workflowName, cronExpr, last.Format(time.RFC3339), gap.Round(time.Second))
	}
}

func (a *Agent) Start(ctx context.Context) error {

	if a.inBox != nil {
		a.inBox.Listen(ctx, func(msg InBox) {
			if err := a.handleMessage(msg); err != nil {
				log.Println("handleMessage error:", err)
			}
		})
	}

	for _, m := range a.messengers {
		go func(m messaging.Messenger) {
			defer recoverFromPanic(a.Name, "messenger "+m.Platform()+" listen loop")
			if err := m.Listen(ctx, func(msg messaging.InboundMessage) {
				a.EnqueueMessage(msg.Conversation, msg.Text, msg.MessageID, msg.ThreadID, msg.WorkflowID)
			}); err != nil {
				log.Printf("messenger %s listen error: %v", m.Platform(), err)
			}
		}(m)
	}

	workflows := a.Workflows
	for _, wf := range workflows {
		switch wf.Trigger.Type {
		case triggers.CronTriggerType:
			cronExpr, ok := wf.Trigger.Value.(string)
			if !ok {
				log.Printf("⚠️%s: workflow %q has CronTriggerType but Value isn't a string (got %T) — skipping", a.Name, wf.Name, wf.Trigger.Value)
				continue
			}
			warnIfCronRunLikelyMissed(a.Name, wf.Name, cronExpr)
			s, _ := gocron.NewScheduler()
			cronJob := gocron.CronJob(cronExpr, false)
			task := gocron.NewTask(func() {
				defer recoverFromPanic(a.Name, "cron-triggered workflow "+wf.Name)

				log.Printf("🧨%s for %s Triggered", wf.Name, a.Name)

				if a.eventBus != nil {
					a.eventBus.PublishEvent("workflow.triggered", a.Name, wf.Name, "Workflow triggered", nil, nil)
				}

				// a.messagingTools() — a cron-triggered workflow has no
				// human waiting in an active conversation, but it still needs
				// send_message to actually deliver anything (the movie
				// recommendation workflow's whole point). resolveSendTarget
				// falls back to the messenger's DefaultConversation() when,
				// as here, ctx carries no active ConversationRef.
				result, err := a.runWorkflow(context.Background(), wf, a.messagingTools(), "You were triggered on your scheduled cron. Run now.")
				if err != nil {
					log.Printf("⚠️%s: cron-triggered workflow %q failed: %v", a.Name, wf.Name, err)
					if a.eventBus != nil {
						a.eventBus.PublishEvent("workflow.failed", a.Name, wf.Name, "Workflow failed", err, nil)
					}
					return
				}

				log.Printf("🧨%s: cron-triggered workflow %q finished (%s): %s", a.Name, wf.Name, result.Status, result.Content)
				if a.eventBus != nil {
					a.eventBus.PublishEvent("workflow.completed", a.Name, wf.Name, fmt.Sprintf("status=%s", result.Status), nil, nil)
				}
			})
			job, err := s.NewJob(cronJob, task)
			if err != nil {
				return err
			}

			s.Start()

			// Logged so a missed run is diagnosable from the deploy that
			// registered it, not just discoverable after the fact by its
			// absence — this is what a job actually believes its next fire
			// time is, right after Start(), not a re-derivation of the cron
			// expression like warnIfCronRunLikelyMissed above.
			if next, err := job.NextRun(); err != nil {
				log.Printf("⚠️%s: workflow %q's cron job has no computable next run: %v", a.Name, wf.Name, err)
			} else {
				log.Printf("🗓️%s: workflow %q next scheduled for %s", a.Name, wf.Name, next.Format(time.RFC3339))
			}

		case triggers.WebhookTriggerType:
			src, ok := wf.Trigger.Value.(webhook.Source)
			if !ok {
				log.Printf("⚠️%s: workflow %q has WebhookTriggerType but Value isn't a webhook.Source (got %T) — skipping", a.Name, wf.Name, wf.Trigger.Value)
				continue
			}
			a.registerWebhook(wf, src)
		}
	}

	log.Printf("🤖%s started successfully", a.Name)

	if a.eventBus != nil {
		a.eventBus.PublishEvent("agent.started", a.Name, "", "Agent started successfully", nil, nil)
	}

	return nil
}

// registerWebhook wires a single workflow onto the shared webhook mux at
// src.Path() — the WebhookTriggerType counterpart to the cron case just
// above it, same shape: log, publish workflow.triggered, run the workflow,
// log + publish workflow.completed/workflow.failed. The one real difference
// is when it fires: cron's task runs on a timer; this runs whenever a
// verified, decoded HTTP request arrives at src.Path(). a.webhookMux must
// already be set (via SetWebhookMux, wired by Runtime.RegisterAgent) or this
// is a no-op — same fail-quiet-and-log stance the cron case takes when
// gocron isn't available.
func (a *Agent) registerWebhook(wf *workflow.Workflow, src webhook.Source) {
	if a.webhookMux == nil {
		log.Printf("⚠️%s: workflow %q has a webhook trigger but no mux configured — skipping", a.Name, wf.Name)
		return
	}

	// running guards against a burst of webhook deliveries (e.g. Gmail's
	// push notifications aren't scoped to exactly one change, and can
	// arrive several at once) spawning overlapping concurrent runs of the
	// same workflow. Every one of these workflows re-derives its own
	// current state from scratch on each run (list_todays_unread_email,
	// get_open_trades, etc.), so a delivery that arrives while a run is
	// already in flight is safe to just drop — the in-flight run (or the
	// next delivery after it finishes) will see whatever that dropped
	// delivery would have. One flag per (workflow, source) registration,
	// not per-agent, since registerWebhook is called once per such pair.
	var running atomic.Bool

	a.webhookMux.HandleFunc(src.Path(), func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		if !src.Verify(body, r.Header) {
			log.Printf("webhook %s: signature verification failed", src.Path())
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		userTrigger, ok, err := src.Decode(body, r.Header)
		if err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}

		if !running.CompareAndSwap(false, true) {
			log.Printf("%s: workflow %q already running from an earlier webhook delivery — skipping this one", a.Name, wf.Name)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Async: a webhook delivery typically has its own short timeout,
		// while a workflow's own loop can run far longer — same reasoning
		// as every hand-rolled webhook handler this replaces.
		go func() {
			defer running.Store(false)
			defer recoverFromPanic(a.Name, "webhook-triggered workflow "+wf.Name)

			log.Printf("🧨%s for %s Triggered", wf.Name, a.Name)
			if a.eventBus != nil {
				a.eventBus.PublishEvent("workflow.triggered", a.Name, wf.Name, "Workflow triggered", nil, nil)
			}

			result, err := a.runWorkflow(context.Background(), wf, a.messagingTools(), userTrigger)
			if err != nil {
				log.Printf("⚠️%s: webhook-triggered workflow %q failed: %v", a.Name, wf.Name, err)
				if a.eventBus != nil {
					a.eventBus.PublishEvent("workflow.failed", a.Name, wf.Name, "Workflow failed", err, nil)
				}
				return
			}

			log.Printf("🧨%s: webhook-triggered workflow %q finished (%s): %s", a.Name, wf.Name, result.Status, result.Content)
			if a.eventBus != nil {
				a.eventBus.PublishEvent("workflow.completed", a.Name, wf.Name, fmt.Sprintf("status=%s", result.Status), nil, nil)
			}
		}()

		w.WriteHeader(http.StatusOK)
	})

	log.Printf("%s: webhook workflow %q registered at %s", a.Name, wf.Name, src.Path())
}

// capabilitiesSummary lists this agent's tools and workflows, one per line,
// with no framing around them — shared between this agent's own system
// prompt (capabilitiesBlock) and how it's described to a sibling agent
// deciding whether to delegate to it (delegateToolSpec).
func (a *Agent) capabilitiesSummary() string {
	var b strings.Builder
	for _, t := range a.baseTools() {
		if t.Name == "end_loop" {
			continue
		}
		_, _ = fmt.Fprintf(&b, "- %s\n", t.Description)
	}
	for _, wf := range a.Workflows {
		_, _ = fmt.Fprintf(&b, "- %s\n", wf.Description)
	}
	return b.String()
}

func (a *Agent) capabilitiesBlock() string {
	var b strings.Builder
	b.WriteString("These are your capabilities, strictly based on tools and workflows:\n")
	b.WriteString(a.capabilitiesSummary())
	b.WriteString("\nWhen asked what you can do, describe your capabilities strictly in terms of the tools and workflows listed above. Do not claim abilities you don't have. If asked to do something outside your available tools, say so honestly rather than implying you can help.")
	return b.String()
}

func (a *Agent) systemPrompt() string {
	return a.IdentityPrompt + "\n" + a.humanBlock() + a.capabilitiesBlock()
}

// humanBlock describes the person this agent serves, when a Human is
// wired in (see SetHuman) — read fresh on every call, so an implementation
// backed by a live cache stays current without this needing to know
// anything about how. Empty string if a.human is nil or Details() returns
// "", so it's safe to always splice into a prompt unconditionally.
// workflowToolSpec's nested run builds its own separate prompt from
// wf.SystemPrompt rather than calling systemPrompt() — see runWorkflow,
// which appends this explicitly for that reason.
func (a *Agent) humanBlock() string {
	if a.human == nil {
		return ""
	}
	details := a.human.Details()
	if details == "" {
		return ""
	}
	return "These are the human's details: " + details + "\n"
}

// delegationBlock lists sibling agents available via delegateToolSpecs, so
// the message-handling path's system prompt actually mentions delegation as
// an option — without it, capabilitiesBlock's "strictly based on tools and
// workflows listed above" instruction leads the model to disclaim a
// delegate tool it technically has schema access to but was never told
// about in prose. Kept separate from capabilitiesSummary (used by
// delegateToolSpec to describe an agent to its siblings) rather than folded
// into it: capabilitiesSummary must stay a leaf computation — if it also
// listed delegates, two agents that can delegate to each other would each
// build the other's tool description by calling back into their own,
// recursing forever.
func (a *Agent) delegationBlock() string {
	delegates := a.delegateToolSpecs()
	if len(delegates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nYou can also delegate to these other agents when a request fits their capabilities better than your own:\n")
	for _, d := range delegates {
		_, _ = fmt.Fprintf(&b, "- %s\n", d.Description)
	}
	return b.String()
}

// RunLoop is the agent-to-user entry point: the only way a real
// conversation (a Telegram message, or anything else that carries a
// ConversationRef) runs an agent's loop. It seeds the loop with this
// conversation's prior turns (if a.memory is set, keyed by the agent's
// configured MemoryKeyFunc — see SetMemoryKeyFunc) and persists the new
// turns back afterward, so the agent remembers earlier messages under that
// same key. Its toolset always includes send_message (if a messenger's
// registered), every workflow as a tool, and every sibling agent as a
// delegate_to_<id> tool — unconditionally, since this method is never used
// for anything but a real conversation. The agent-to-agent counterpart is
// runDelegatedTask, a genuinely separate method rather than a flagged
// variant of this one.
// ResolvedTools returns this agent's complete, merged toolset for a given
// platform — base tools, every workflow as a tool, and every sibling agent
// as a delegate_to_<id> tool — the same assembly RunLoop feeds its own LLM
// call, exposed as a reusable method so something outside the loop (e.g. an
// MCP server bridging this agent's tools to an external caller) can get the
// exact same list without duplicating the merge/filter logic.
func (a *Agent) ResolvedTools(platform string) []*tools.ToolSpec {
	toolset := mergeTools(a.baseTools(), a.workflowToolSpecs(a.messagingTools()), a.delegateToolSpecs())
	return filterToolsForPlatform(toolset, platform)
}

func (a *Agent) RunLoop(ctx context.Context, conv messaging.ConversationRef, userPrompt string) (*LoopResult, error) {
	ctx = ensureDelegationNotes(ctx)
	memKey := a.memoryKey(ctx, conv)

	var prior []llm.Message
	if a.memory != nil {
		prior = a.memory.History(ctx, memKey)
	}

	messages := make([]llm.Message, 0, len(prior)+2)
	messages = append(messages, llm.Message{Role: "system", Content: a.systemPrompt() + a.delegationBlock()})
	messages = append(messages, prior...)
	messages = append(messages, llm.Message{Role: "user", Content: userPrompt})

	toolset := a.ResolvedTools(conv.Platform)
	result, final, err := a.runLoopFrom(ctx, messages, toolset, a.MaxIterations, a.MaxDuplicateToolCallsPerResponse, a.MaxToolCallsPerResponse, maxSameToolCallsPerRun)

	if a.memory != nil {
		// Persist only what this call actually added — everything after the
		// fresh system message and the prior turns we already had stored.
		newTurns := final[1+len(prior):]
		a.memory.Append(ctx, memKey, newTurns...)
	}

	if err != nil {
		// A genuine infra/LLM failure (Call erroring or timing out) —
		// runLoopFrom returns before producing any reply here, so without
		// this the conversation just goes silent: the error only reaches
		// the logs, and the user has no way to know their message was even
		// received. Best-effort notify instead, same "the user should
		// always hear something back" reasoning as the guaranteed-delivery
		// block below, just for the failure case it doesn't cover.
		if m, ok := a.messengers[conv.Platform]; ok {
			errNotice := consumeDelegationNotes(ctx) + "Sorry, I ran into an error and couldn't finish handling that. Please try again."
			if _, sendErr := a.replyOrSend(ctx, m, conv, errNotice); sendErr != nil {
				log.Println("failed to deliver error notice:", sendErr)
			}
		}
		return result, err
	}

	// Guarantee delivery: the final answer always reaches the user once the
	// loop completes successfully, regardless of whether the model
	// remembered to call send_message along the way — delivery is a
	// structural consequence of the loop finishing, not something that
	// depends on the model choosing the right tool. Skipped if send_message
	// already succeeded this turn, so a model that DID use it to reply
	// doesn't get its answer sent twice. Fires even when result.Content is
	// empty: confirmed live that a model can call end_loop with an empty
	// final_message on a genuinely-completed turn (tool calls all
	// succeeded, nothing more to do) — the old `result.Content != ""` guard
	// silently dropped that case entirely, logging status=completed while
	// the user received nothing at all. A generic fallback beats silence,
	// since the caller has no other signal the run ever happened.
	if err == nil && result.Status == LLMStatusComplete {
		alreadyReplied := false
		for _, m := range final[1+len(prior):] {
			if m.Role == "tool" && m.Name == "send_message" && !strings.HasPrefix(m.Content, "error:") {
				alreadyReplied = true
				break
			}
		}
		if !alreadyReplied {
			if m, ok := a.messengers[conv.Platform]; ok {
				content := result.Content
				if content == "" {
					content = "Done — the task completed, but I didn't leave a summary. Let me know if you'd like more detail."
				}
				finalContent := consumeDelegationNotes(ctx) + content
				if _, sendErr := a.replyOrSend(ctx, m, conv, finalContent); sendErr != nil {
					log.Println("failed to deliver final answer:", sendErr)
				}
			}
		}
	}

	return result, err
}

func (a *Agent) handleMessage(msg InBox) error {
	log.Printf("💬%s: handling message from %s/%s: %q", a.Name, msg.Conversation.Platform, msg.Conversation.ChatID, msg.Payload)

	ctx := messaging.WithConversation(context.Background(), msg.Conversation)
	ctx = messaging.WithMessageID(ctx, msg.MessageID)
	ctx = messaging.WithThreadID(ctx, msg.ThreadID)
	ctx = messaging.WithWorkflowID(ctx, msg.WorkflowID)
	result, err := a.RunLoop(ctx, msg.Conversation, msg.Payload)
	if err != nil {
		return err
	}

	log.Printf("💬%s: finished handling message from %s/%s: status=%s iterations=%d", a.Name, msg.Conversation.Platform, msg.Conversation.ChatID, result.Status, result.Iteration)

	if result.Status == LLMStatusMaxIteration {
		return fmt.Errorf("message handling hit the %d-iteration cap without completing", a.MaxIterations)
	}
	if result.Status == LLMToolCallError {
		return fmt.Errorf("message handling gave up early at iteration %d — either too many consecutive turns with no successful tool call, or one tool called too many times in this run; see the ⚠️ log line just above for which", result.Iteration)
	}
	return nil
}

func mergeTools(sets ...[]*tools.ToolSpec) []*tools.ToolSpec {
	result := make([]*tools.ToolSpec, 0)
	for _, set := range sets {
		result = append(result, set...)
	}
	return result
}

// resolvePlatform determines which platform the current run is actually on,
// so platform-scoped tools (ToolSpec.Platform) can be filtered before they
// ever reach the LLM — a Slack-only tool offered during a Telegram
// conversation would either error at dispatch or just confuse the model.
// Mirrors resolveSendTarget's own fallback: the active conversation from
// ctx if one's attached (a real inbound message, or a delegated call that
// inherited it), otherwise the first registered messenger's default
// conversation (a cron-triggered workflow, with nothing to reply to yet but
// still a known eventual destination). Returns "" if neither resolves (no
// messenger registered at all), in which case every platform-scoped tool is
// filtered out — the safe default when it's genuinely unknown where, if
// anywhere, a reply could go.
func (a *Agent) resolvePlatform(ctx context.Context) string {
	if conv, ok := messaging.ConversationFromContext(ctx); ok {
		return conv.Platform
	}
	if conv, ok := a.DefaultConversation(); ok {
		return conv.Platform
	}
	return ""
}

// filterToolsForPlatform drops any tool scoped to a different platform than
// the one this run is actually on. A tool with no Platform set (the
// default) is unrestricted and always kept.
func filterToolsForPlatform(toolset []*tools.ToolSpec, platform string) []*tools.ToolSpec {
	filtered := make([]*tools.ToolSpec, 0, len(toolset))
	for _, t := range toolset {
		if t.Platform == "" || t.Platform == platform {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func toolMapValues(m map[string]*tools.ToolSpec) []*tools.ToolSpec {
	result := make([]*tools.ToolSpec, 0, len(m))
	for _, t := range m {
		result = append(result, t)
	}
	return result
}

// workflowToolSpecs turns every workflow the agent owns into a callable
// tool, so the loop can decide to run one instead of freehanding the task
// with tools that weren't meant for it. messagingTools is passed straight
// through to each workflow's own nested run (see runWorkflow) — the
// agent's real a.messagingTools() from RunLoop (a real conversation, where
// replying to a human directly is legitimate), nil from runDelegatedTask
// (a workflow triggered mid-delegation has no live, identity-matched
// conversation to message into, same reasoning as the delegated call
// itself).
func (a *Agent) workflowToolSpecs(messagingTools []*tools.ToolSpec) []*tools.ToolSpec {
	specs := make([]*tools.ToolSpec, 0, len(a.Workflows))
	for _, wf := range a.Workflows {
		specs = append(specs, a.workflowToolSpec(wf, messagingTools))
	}
	return specs
}

// runWorkflow runs wf's own nested loop with userTrigger as the user turn —
// shared by workflowToolSpec (triggered mid-conversation, by the model
// calling it like any other tool) and Start's cron scheduler (triggered on
// a timer, no model involved in the decision to run it at all), so both
// paths behave identically once a run actually starts: same system prompt
// assembly, same toolset rules, same iteration cap resolution.
// delegationDepthKey is the ctx key a hop count is carried under — see
// incrementDelegationDepth.
type delegationDepthKey struct{}

// maxDelegationDepth bounds how many delegate/workflow hops a single call
// chain can take before it's rejected outright. Not a tight bound: a
// legitimate chain (conversation → delegate → target's own workflow →
// delegate again) is already 4 hops without anything unusual happening, so
// this exists to catch a genuinely circular configuration — e.g. Agent A's
// AllowDelegation workflow delegating to Agent B, whose own AllowDelegation
// workflow delegates back to Agent A — failing loudly within a few hops
// rather than after dozens of nested, expensive LLM loops.
const maxDelegationDepth = 4

// incrementDelegationDepth reads the current hop count off ctx (0 if unset),
// returns a context carrying count+1, and reports whether the NEW count is
// still within maxDelegationDepth. Called at the top of both runWorkflow and
// runDelegatedTask — the only two entry points that can lead to another
// delegate/workflow hop.
func incrementDelegationDepth(ctx context.Context) (context.Context, int, bool) {
	depth, _ := ctx.Value(delegationDepthKey{}).(int)
	depth++
	return context.WithValue(ctx, delegationDepthKey{}, depth), depth, depth <= maxDelegationDepth
}

// workflowToolset composes wf's toolset for one run: a.Tools (everything
// added via AddTool, including any RequiresApproval tool), retrievers, and
// thread-history are unconditional — none of them depend on there being a
// live conversation, so none of them are ever gated. messagingTools
// (send_message and any messenger-contributed tools, see
// Agent.messagingTools) is the only conditional piece, supplied by the
// caller — nil for a run with no live, identity-matched conversation to
// speak into as itself (see runDelegatedTask and the nested workflow-as-
// tool calls it makes). Pulled out of runWorkflow so the composition
// itself — specifically, that a.Tools can never end up excluded — is
// directly testable without running an actual LLM loop.
func (a *Agent) workflowToolset(wf *workflow.Workflow, messagingTools []*tools.ToolSpec) []*tools.ToolSpec {
	base := append(a.defaultTools(), messagingTools...)
	toolset := mergeTools(base, toolMapValues(wf.Tools))
	if wf.AllowDelegation {
		toolset = mergeTools(toolset, a.delegateToolSpecs())
	}
	return toolset
}

func (a *Agent) runWorkflow(ctx context.Context, wf *workflow.Workflow, messagingTools []*tools.ToolSpec, userTrigger string) (*LoopResult, error) {
	var depth int
	var withinLimit bool
	ctx, depth, withinLimit = incrementDelegationDepth(ctx)
	if !withinLimit {
		return nil, fmt.Errorf("workflow %q: delegation depth exceeded (%d > %d) — likely a delegation cycle across workflows", wf.Name, depth, maxDelegationDepth)
	}

	// Threaded here (not left to runLoopFrom, which does this too but only
	// once it's already running) so wf.MaxToolCallsPerResponseFunc and
	// wf.MaxDuplicateToolCallsPerResponseFunc below can resolve identities
	// and retrievers via ctx exactly the way a real tool handler does —
	// e.g. minting a fresh API token to ask "how many items am I about to
	// fan out over" before the run even starts.
	ctx = identity.WithIdentities(ctx, a.identities)
	ctx = rag.WithRetrievers(ctx, a.retrievers)
	ctx = ensureDelegationNotes(ctx)
	ctx = messaging.WithWorkflowID(ctx, wf.ID)

	// conv: cron/webhook triggers pass context.Background(), so there's no
	// conversation to inherit — fall back to DefaultConversation. Triggered
	// mid-conversation as a tool call (workflowToolSpec's handler, calling
	// this with the SAME ctx it received), ctx already carries the real
	// conversation from RunLoop, so that's preferred instead. With
	// KeyByWorkflow configured, WorkflowID above wins over either case
	// anyway — this only matters for a MemoryKeyFunc that doesn't wrap it.
	conv, ok := messaging.ConversationFromContext(ctx)
	if !ok {
		conv, _ = a.DefaultConversation()
	}
	memKey := a.memoryKey(ctx, conv)

	var prior []llm.Message
	if a.memory != nil {
		prior = a.memory.History(ctx, memKey)
	}

	messages := make([]llm.Message, 0, len(prior)+2)
	messages = append(messages, llm.Message{Role: "system", Content: a.humanBlock() + wf.SystemPrompt + " " + loopProtocolInstructions})
	messages = append(messages, prior...)
	messages = append(messages, llm.Message{Role: "user", Content: userTrigger})
	toolset := a.workflowToolset(wf, messagingTools)
	toolset = filterToolsForPlatform(toolset, a.resolvePlatform(ctx))

	maxIter := wf.Iteration
	if maxIter <= 0 {
		maxIter = a.MaxIterations
	}
	maxDuplicateToolCalls := wf.MaxDuplicateToolCallsPerResponse
	if wf.MaxDuplicateToolCallsPerResponseFunc != nil {
		if v, err := wf.MaxDuplicateToolCallsPerResponseFunc(ctx); err != nil {
			log.Printf("⚠️%s: workflow %q's MaxDuplicateToolCallsPerResponseFunc failed, falling back to its static value: %v", a.Name, wf.Name, err)
		} else {
			maxDuplicateToolCalls = v
		}
	}
	if maxDuplicateToolCalls <= 0 {
		maxDuplicateToolCalls = a.MaxDuplicateToolCallsPerResponse
	}
	maxToolCalls := wf.MaxToolCallsPerResponse
	if wf.MaxToolCallsPerResponseFunc != nil {
		if v, err := wf.MaxToolCallsPerResponseFunc(ctx); err != nil {
			log.Printf("⚠️%s: workflow %q's MaxToolCallsPerResponseFunc failed, falling back to its static value: %v", a.Name, wf.Name, err)
		} else {
			maxToolCalls = v
		}
	}
	if maxToolCalls <= 0 {
		maxToolCalls = a.MaxToolCallsPerResponse
	}
	result, final, err := a.runLoopFrom(ctx, messages, toolset, maxIter, maxDuplicateToolCalls, maxToolCalls, maxIter)
	if a.memory != nil {
		newTurns := final[1+len(prior):]
		a.memory.Append(ctx, memKey, newTurns...)
	}
	return result, err
}

// workflowToolSpec wraps a single workflow as a tool. Invoking it runs a
// nested loop (via runWorkflow) scoped to that workflow's own system
// prompt and tools — a.Tools, retrievers, and thread-history unconditionally,
// plus messagingTools (send_message and any messenger-contributed tools)
// when the caller supplies them — plus delegate_to_<sibling> tools when
// wf.AllowDelegation is set — off by default, so most workflows still see
// only their own tools, and runWorkflow's depth guard bounds how far any
// resulting delegation chain can actually go.
func (a *Agent) workflowToolSpec(wf *workflow.Workflow, messagingTools []*tools.ToolSpec) *tools.ToolSpec {
	return tools.NewToolBuilder(wf.ID, wf.Description).
		Parameter("context", "string", "Any extra detail from the user's message relevant to this workflow (optional).", false).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			userTrigger := "Run this workflow now."
			if args != nil {
				var raw string
				if err := json.Unmarshal(args, &raw); err == nil {
					var input struct {
						Context string `json:"context"`
					}
					if err := json.Unmarshal([]byte(raw), &input); err == nil && input.Context != "" {
						userTrigger = input.Context
					}
				}
			}

			result, err := a.runWorkflow(ctx, wf, messagingTools, userTrigger)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("workflow %q finished (%s): %s", wf.Name, result.Status, result.Content), nil
		}).Build()
}

// delegateToolSpecs turns every sibling agent visible through a.directory
// into a callable tool, so a chat loop can hand off a task instead of
// attempting it with tools that weren't meant for it. Nil directory (no
// Runtime wired in — the common case for a single standalone agent) yields
// no delegate tools, same as an agent with zero workflows gets none of
// those either.
func (a *Agent) delegateToolSpecs() []*tools.ToolSpec {
	if a.directory == nil {
		return nil
	}
	specs := make([]*tools.ToolSpec, 0)
	for _, sibling := range a.directory.GetAgents() {
		if sibling.Id == a.Id {
			continue
		}
		specs = append(specs, a.delegateToolSpec(sibling))
	}
	return specs
}

// delegateToolSpec wraps a single sibling agent as a tool. Its description
// carries the sibling's name, description, and its own tools/workflows
// (via capabilitiesSummary) so the calling LLM can judge what it's actually
// good for, not just guess from a one-line blurb. Invoking it runs
// target.runDelegatedTask — the agent-to-agent path, not RunLoop — so
// there's no ConversationRef, no memory, and no send_message involved at
// all; see runDelegatedTask for why.
func (a *Agent) delegateToolSpec(target *Agent) *tools.ToolSpec {
	description := fmt.Sprintf(
		"Delegate a task to %s: %s\nIts capabilities:\n%s",
		target.Name, target.Description, target.capabilitiesSummary(),
	)
	return tools.NewToolBuilder("delegate_to_"+target.Id, description).
		Parameter("task", "string", "What you want this agent to do.", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			if args == nil {
				return nil, fmt.Errorf("delegate_to_%s: no arguments provided", target.Id)
			}

			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}

			var input struct {
				Task string `json:"task"`
			}

			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			log.Printf("🤝%s: delegating to %s: %q", a.Name, target.Name, input.Task)
			recordDelegation(ctx, target)
			result, err := target.runDelegatedTask(messaging.WithDelegator(ctx, a.Id), input.Task)
			if err != nil {
				return nil, err
			}
			log.Printf("🤝%s: %s finished (%s): %s", a.Name, target.Name, result.Status, result.Content)
			return fmt.Sprintf("%s finished (%s): %s", target.Name, result.Status, result.Content), nil
		}).Build()
}

// delegationNotesCtxKey is the context key for the mutable notes collector
// ensureDelegationNotes/recordDelegation/consumeDelegationNotes share.
type delegationNotesCtxKey struct{}

// ensureDelegationNotes attaches a mutable delegation-notes collector to
// ctx if one isn't already present, so delegateToolSpec's handler (deep
// inside runLoopFrom, possibly several nested workflow-as-tool calls down)
// has somewhere to record what it delegated. Idempotent by design: a
// nested runWorkflow call (workflow-as-tool from within an outer RunLoop)
// reuses the outer collector rather than shadowing it with its own, so a
// delegation made deep inside a nested workflow still surfaces in the
// outer conversation's one reply. Only for same-agent nesting — see
// resetDelegationNotes for the agent-to-agent boundary, which must NOT
// reuse the caller's collector.
func ensureDelegationNotes(ctx context.Context) context.Context {
	if _, ok := ctx.Value(delegationNotesCtxKey{}).(*[]string); ok {
		return ctx
	}
	return context.WithValue(ctx, delegationNotesCtxKey{}, &[]string{})
}

// resetDelegationNotes always attaches a brand-new, empty notes collector
// to ctx, discarding any inherited one. Used at the agent-to-agent boundary
// (runDelegatedTask), unlike ensureDelegationNotes's reuse-if-present
// behavior: a delegate's own reply is a separate message from whatever the
// delegating agent eventually sends, so a note recorded for the
// delegator's own pending reply must never leak into — or be silently
// consumed by — the delegate's own optional send_message call. Without
// this, ctx (and the collector it carries) flows straight from
// delegateToolSpec's handler into the delegate's own runLoopFrom, and the
// delegate's optional direct send_message ends up eating the note meant
// for the delegator's final relay, leaving the delegator's own message
// with no note at all.
func resetDelegationNotes(ctx context.Context) context.Context {
	return context.WithValue(ctx, delegationNotesCtxKey{}, &[]string{})
}

// recordDelegation notes that this run handed a task to target, to be
// folded into the single outgoing reply (see consumeDelegationNotes)
// rather than sent as a message of its own — deliberately not left to the
// delegating agent's own judgment on whether to mention it when it
// eventually relays an answer. Models have already been observed, twice,
// not reliably using an *optional* tool (a reaction, a direct
// send_message) even when explicitly told they're allowed to — so this
// bypasses the LLM entirely: it always fires from delegateToolSpec's
// handler, not something the model chooses to do. No-op if ctx was never
// wrapped by ensureDelegationNotes (shouldn't happen on any real call
// path, but a missing note beats a panic). The task itself is deliberately
// left out of the note — it's already in the log line right above this
// call, and repeating it here would just restate what the reply right
// after it already shows.
func recordDelegation(ctx context.Context, target *Agent) {
	ptr, ok := ctx.Value(delegationNotesCtxKey{}).(*[]string)
	if !ok {
		return
	}
	*ptr = append(*ptr, fmt.Sprintf("🤝 Handed this off to %s", target.Name))
}

// consumeDelegationNotes returns any recorded delegation notes formatted
// as a prefix for the outgoing reply, and clears them. Called by every
// path that can actually deliver a reply (send_message's handler and
// RunLoop's guaranteed-delivery fallback) so the note appears exactly
// once, attached to whichever message ends up being the real answer,
// instead of as a separate message of its own.
func consumeDelegationNotes(ctx context.Context) string {
	ptr, ok := ctx.Value(delegationNotesCtxKey{}).(*[]string)
	if !ok || len(*ptr) == 0 {
		return ""
	}
	prefix := strings.Join(*ptr, "\n") + "\n\n"
	*ptr = (*ptr)[:0]
	return prefix
}

// runDelegatedTask is the agent-to-agent counterpart to RunLoop's
// agent-to-user path — kept as a genuinely separate method rather than a
// flagged variant of RunLoop, so the distinction is visible in the type
// signature instead of hidden in context. Differences from RunLoop, each
// structural rather than a withheld/flagged behavior:
//   - No ConversationRef parameter: delegation isn't a human conversation,
//     so it doesn't borrow that type to pretend it is. Memory is still
//     keyed via a.memoryKey(ctx, conv), using whatever ConversationRef (and
//     WorkflowID, if any) the delegating call inherited via ctx — each
//     delegate applies its own configured MemoryKeyFunc to that inherited
//     context, in its own memory store, so e.g. a delegation traced back to
//     a specific workflow gets its own accumulating history on the
//     delegate's side too, distinct from a plain one-off delegated call.
//   - Its toolset is defaultTools() + messengerTools() +
//     workflowToolSpecs(nil) — everything a normal conversational run
//     gets EXCEPT send_message itself: a delegate has no live,
//     identity-matched conversation to post into as its own bot (see
//     Agent.messagingTools) — its one guaranteed way back is
//     end_loop's final_message, relayed by the delegator. add_reaction
//     (or another messenger-contributed tool) stays available, since the
//     system prompt below offers it as a lightweight status signal, not
//     a reply mechanism. No delegateToolSpecs() either, so no further
//     delegation from this agent's own direct turn — preventing a cycle
//     (a nested workflow this delegate calls can still delegate further
//     if it opts in via AllowDelegation; incrementDelegationDepth is
//     what actually bounds that, not a blanket ban).
func (a *Agent) runDelegatedTask(ctx context.Context, task string) (*LoopResult, error) {
	var depth int
	var withinLimit bool
	ctx, depth, withinLimit = incrementDelegationDepth(ctx)
	if !withinLimit {
		return nil, fmt.Errorf("%s: delegation depth exceeded (%d > %d) — likely a delegation cycle across workflows", a.Name, depth, maxDelegationDepth)
	}
	ctx = resetDelegationNotes(ctx)

	// conv: inherited from whoever delegated to us (the delegator's own
	// ConversationRef, or nothing if the delegator itself had none — see
	// runWorkflow's fallback to DefaultConversation, which this then
	// inherits in turn).
	conv, _ := messaging.ConversationFromContext(ctx)
	memKey := a.memoryKey(ctx, conv)

	var prior []llm.Message
	if a.memory != nil {
		prior = a.memory.History(ctx, memKey)
	}

	systemContent := a.systemPrompt() +
		"\n\nThis task was delegated to you by another agent, not requested directly by a human. Always call end_loop with your final_message when done — it is relayed back to the delegating agent automatically, and is the one guaranteed way your answer gets through, so the delegating agent can pass it on itself. If a reaction tool (e.g. add_reaction) is available, you may use it on the conversation/message that triggered this delegation as a lightweight status signal — not a replacement for final_message."

	messages := make([]llm.Message, 0, len(prior)+2)
	messages = append(messages, llm.Message{Role: "system", Content: systemContent})
	messages = append(messages, prior...)
	messages = append(messages, llm.Message{Role: "user", Content: task})
	toolset := mergeTools(a.defaultTools(), a.messengerTools(), a.workflowToolSpecs(nil))
	toolset = filterToolsForPlatform(toolset, a.resolvePlatform(ctx))

	result, final, err := a.runLoopFrom(ctx, messages, toolset, a.MaxIterations, a.MaxDuplicateToolCallsPerResponse, a.MaxToolCallsPerResponse, maxSameToolCallsPerRun)
	if a.memory != nil {
		newTurns := final[1+len(prior):]
		a.memory.Append(ctx, memKey, newTurns...)
	}
	return result, err
}

type LoopResult struct {
	Iteration int       `json:"iteration"`
	Status    LLMStatus `json:"status"`
	Content   string    `json:"content,omitempty"`
}
