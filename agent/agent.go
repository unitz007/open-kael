package agent

import (
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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-co-op/gocron/v2"
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
	LLM            llm.LLM
	Tools          []*tools.ToolSpec `json:"tools"`
	eventBus       EventPublisher
	memory         Memory
	inBox          *MessageQueue
	human          *human.Human
	messengers     map[string]messaging.Messenger
	directory      AgentDirectory
	webhookMux     *http.ServeMux
	identities     map[string]identity.Identity
	retrievers     map[string]rag.Retriever
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

// maxToolCallsPerResponse bounds how many tool calls a single LLM response
// is allowed to contain. A legitimate turn needs at most a handful; far
// more than that is a model decoding glitch (stuck repeating the same
// output), not real multi-tool intent — see the check in runLoopFrom.
const maxToolCallsPerResponse = 10

// runLoopFrom is the shared tool-calling engine behind every entry point
// (RunLoop, a workflow's own nested run, and a delegated call) — same
// end_loop protocol, same repeat-call guard, just fed a different starting
// transcript, a different tool set, and (since a workflow can legitimately
// need more or fewer steps than a plain conversation) a different iteration
// cap. It returns the final transcript too, so a caller that persists
// conversation history (RunLoop) can see exactly what turns were added. ctx
// flows to every tool call unchanged — this is how a conversation attached
// via messaging.WithConversation reaches send_message's handler.
func (a *Agent) runLoopFrom(ctx context.Context, messages []llm.Message, toolset []*tools.ToolSpec, maxIter int) (*LoopResult, []llm.Message, error) {
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

	// maxIter <= 0 means unlimited — the loop then only ever ends via
	// end_loop or an LLM.Call error, never by exhausting a count. There's
	// no other backstop: LLM.Call takes no ctx, so cancelling the caller's
	// context doesn't interrupt an in-flight or subsequent call either.
	// The repeat-call guard above and the runaway-tool-call-batch check
	// below still apply exactly as they do with a normal cap — this only
	// removes the iteration count as a termination condition.
	for i := 0; maxIter <= 0 || i < maxIter; i++ {
		response, err := a.LLM.Call(messages, toolset)
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
			continue
		}

		if len(response.ToolCalls) > maxToolCallsPerResponse {
			// A real turn needs at most a handful of tool calls. A response
			// with dozens or hundreds — all identical, in one shot — isn't
			// legitimate multi-tool use, it's a decoding glitch (some models
			// get stuck repeating the same output token sequence). Left
			// unchecked, processing all of them floods the log with
			// "blocked" repeats (only the first of any given call actually
			// runs) and bloats the transcript pointlessly. Discard the whole
			// batch without executing or recording any of it, and nudge for
			// a single deliberate call instead — same "consume an iteration,
			// try again" shape as the tool-less-response case above.
			log.Printf("⚠️%s's response contained %d tool calls in one turn (cap %d) — discarding as a decoding glitch rather than executing any of them", a.Name, len(response.ToolCalls), maxToolCallsPerResponse)
			messages = append(messages,
				llm.Message{Role: "assistant", Content: response.Content},
				llm.Message{Role: "user", Content: "That response repeated tool calls an unreasonable number of times in one turn, so none of them were executed. Try again with one single, deliberate tool call."},
			)
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
				continue
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
				continue
			}

			result, err := tool.Handler(ctx, toolCall.Arguments)
			if err != nil {
				log.Println("Tool execution error:", err.Error())
				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: toolCall.Id,
					Name:       tool.Name,
					Content:    fmt.Sprintf("error: %v", err),
				})
				continue
			}
			calledBefore[callKey] = true

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

			//log.Printf("Tool %s executed successfully with result: %v", tool.Name, result)

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

// SetMemory overrides this agent's conversation memory — NewAgent already
// sets up a working default (file-backed, falling back to in-memory-only
// on any filesystem error), so this exists purely for swapping in a
// different backing store, e.g. one backed by a database instead of local
// disk. Call before Start (or before the first RunLoop) — a change mid
// conversation would silently orphan whatever was already recorded in the
// old store.
func (a *Agent) SetMemory(m Memory) {
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
// "codebase" and "docs". Same reasoning as IdentifyAs: a tool doesn't take
// a Retriever directly, since a workflow's tools are built before the
// owning agent even exists — they resolve whichever retriever they need at
// call time via rag.FromContext, since runLoopFrom threads a.retrievers
// into ctx on every run.
func (a *Agent) AddRetriever(name string, r rag.Retriever) {
	if a.retrievers == nil {
		a.retrievers = make(map[string]rag.Retriever)
	}
	a.retrievers[name] = r
}

// EnqueueMessage is the external entry point for feeding a message to this
// agent — main.go, a Messenger's Listen callback, or a test. Safe to call
// before Start(): Enqueue just buffers on the channel regardless of whether
// a listener is attached yet. InBox/MessageQueue stay internal; callers
// never need to touch them directly.
func (a *Agent) EnqueueMessage(conv messaging.ConversationRef, text, messageID, threadID string) {
	a.inBox.Enqueue(InBox{Conversation: conv, Payload: text, MessageID: messageID, ThreadID: threadID})
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
const loopProtocolInstructions = "You have access to tools. " +
	"When you have fully completed the request and have nothing further to do, call the end_loop tool. " +
	"Do not call end_loop while you still have pending work. " +
	"Put your actual answer in end_loop's final_message argument — every response must call a tool, so that is the only place your answer is captured. " +
	"Never repeat a tool call with the same arguments after it has already succeeded — one send is enough. As soon as an action succeeds, call end_loop immediately unless explicitly asked for it to happen more than once." +
	"Never expose a tool name or argument in your final answer — the user should not see any tool calls, only the final_message you put in end_loop."

func NewAgent(id, name, description, identityPrompt string, llm llm.LLM) *Agent {
	a := &Agent{
		Id:             id,
		Name:           name,
		Description:    description,
		IdentityPrompt: "You are an AI agent for Charles Dinneya." + identityPrompt + loopProtocolInstructions,
		Workflows:      make([]*workflow.Workflow, 0),
		LLM:            llm,
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
		eventBus:      nil,
		memory:        newAgentMemory(id),
		inBox:         NewMessageQueue(32),
		MaxIterations: defaultMaxIterations,
	}

	return a
}

// newAgentMemory builds this agent's persistent conversation memory — a
// JSON file under data/memory/<id>.json, so history survives a process
// restart instead of vanishing every time, which across this project's own
// development has been almost every code change. Falls back to
// in-memory-only (the old behavior) if the file can't be set up, so a
// filesystem problem degrades this one agent's memory rather than crashing
// the whole runtime.
func newAgentMemory(id string) Memory {
	path := filepath.Join("data", "memory", id+".json")

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		log.Printf("agent %s: could not create memory directory, falling back to in-memory-only history: %v", id, err)
		return memory.NewInMemoryHistory()
	}

	fh, err := memory.NewFileHistory(path)
	if err != nil {
		log.Printf("agent %s: could not load persistent memory from %s, falling back to in-memory-only history: %v", id, path, err)
		return memory.NewInMemoryHistory()
	}
	return fh
}

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
	if len(a.messengers) == 0 {
		return a.Tools
	}
	out := append(append([]*tools.ToolSpec{}, a.Tools...), a.sendMessageTool())
	return append(out, a.messengerTools()...)
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
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			if args == nil {
				return nil, fmt.Errorf("send_message: no arguments provided")
			}

			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}

			var input struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			target, messenger, err := a.resolveSendTarget(ctx)
			if err != nil {
				log.Println("send_message: could not resolve a target:", err)
				return nil, err
			}
			log.Printf("📤%s: routing send_message to %s/%s", a.Name, target.Platform, target.ChatID)

			if err := messenger.Send(ctx, target, input.Message); err != nil {
				log.Println("Error sending message:", err)
				return nil, err
			}
			return "Message sent successfully", nil
		}).Build()
}

// resolveSendTarget picks which conversation/messenger send_message should
// use: the active conversation from ctx if one's attached (replying to an
// inbound message), or the first registered messenger's own default
// conversation otherwise (a proactive send — e.g. a cron-triggered
// workflow with nothing to reply to).
func (a *Agent) resolveSendTarget(ctx context.Context) (messaging.ConversationRef, messaging.Messenger, error) {
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

// DefaultConversation returns the first registered messenger's default
// conversation — where a proactive send with no active conversation in
// context goes. ok is false if no messenger is registered at all. External
// callers (main.go) use this to address an initial message without needing
// to know which platform is actually registered.
func (a *Agent) DefaultConversation() (conv messaging.ConversationRef, ok bool) {
	for _, m := range a.messengers {
		return m.DefaultConversation(), true
	}
	return messaging.ConversationRef{}, false
}

// Memory holds an agent's conversation turns across separate inbound
// messages, keyed by an arbitrary string id.
type Memory interface {
	History(id string) []llm.Message
	Append(id string, messages ...llm.Message)
}

// ownerMemoryKey is the single memory thread every RunLoop call reads from
// and writes to, regardless of which platform or which conversation ID the
// message actually arrived on. Deliberate: this agent is single-owner (one
// person, potentially reachable through several Messengers — e.g. both
// Telegram and Slack), and per-platform IDs never line up across
// platforms, so keying by conv.ChatID would silently start a fresh,
// disconnected thread every time the owner switched platforms. The
// tradeoff, worth remembering if this ever stops being true: nothing
// distinguishes *who* is messaging — anyone able to reach any registered
// Messenger shares this same thread and sees the same history.
const ownerMemoryKey = "owner"

type EventPublisher interface {
	PublishEvent(eventType, agentName, workflow, message string, err error, data map[string]interface{})
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
			if err := m.Listen(ctx, func(msg messaging.InboundMessage) {
				a.EnqueueMessage(msg.Conversation, msg.Text, msg.MessageID, msg.ThreadID)
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
			s, _ := gocron.NewScheduler()
			cronJob := gocron.CronJob(cronExpr, false)
			task := gocron.NewTask(func() {
				log.Printf("🧨%s for %s Triggered", wf.Name, a.Name)

				if a.eventBus != nil {
					a.eventBus.PublishEvent("workflow.triggered", a.Name, wf.Name, "Workflow triggered", nil, nil)
				}

				// includeUserReply: true — a cron-triggered workflow has no
				// human waiting in an active conversation, but it still needs
				// send_message to actually deliver anything (the movie
				// recommendation workflow's whole point). resolveSendTarget
				// falls back to the messenger's DefaultConversation() when,
				// as here, ctx carries no active ConversationRef.
				result, err := a.runWorkflow(context.Background(), wf, true, "You were triggered on your scheduled cron. Run now.")
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
			_, err := s.NewJob(cronJob, task)
			if err != nil {
				return err
			}

			s.Start()

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

		// Async: a webhook delivery typically has its own short timeout,
		// while a workflow's own loop can run far longer — same reasoning
		// as every hand-rolled webhook handler this replaces.
		go func() {
			log.Printf("🧨%s for %s Triggered", wf.Name, a.Name)
			if a.eventBus != nil {
				a.eventBus.PublishEvent("workflow.triggered", a.Name, wf.Name, "Workflow triggered", nil, nil)
			}

			result, err := a.runWorkflow(context.Background(), wf, true, userTrigger)
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
	return a.IdentityPrompt + "\n" + a.capabilitiesBlock()
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
// conversation's prior turns (if a.memory is set, keyed by conv.ChatID) and
// persists the new turns back afterward, so the agent remembers earlier
// messages in the same conversation. Its toolset always includes
// send_message (if a messenger's registered), every workflow as a tool, and
// every sibling agent as a delegate_to_<id> tool — unconditionally, since
// this method is never used for anything but a real conversation. The
// agent-to-agent counterpart is runDelegatedTask, a genuinely separate
// method rather than a flagged variant of this one.
func (a *Agent) RunLoop(ctx context.Context, conv messaging.ConversationRef, userPrompt string) (*LoopResult, error) {
	var prior []llm.Message
	if a.memory != nil {
		prior = a.memory.History(ownerMemoryKey)
	}

	messages := make([]llm.Message, 0, len(prior)+2)
	messages = append(messages, llm.Message{Role: "system", Content: a.systemPrompt() + a.delegationBlock()})
	messages = append(messages, prior...)
	messages = append(messages, llm.Message{Role: "user", Content: userPrompt})

	toolset := mergeTools(a.baseTools(), a.workflowToolSpecs(true), a.delegateToolSpecs())
	toolset = filterToolsForPlatform(toolset, conv.Platform)
	result, final, err := a.runLoopFrom(ctx, messages, toolset, a.MaxIterations)

	if a.memory != nil {
		// Persist only what this call actually added — everything after the
		// fresh system message and the prior turns we already had stored.
		newTurns := final[1+len(prior):]
		a.memory.Append(ownerMemoryKey, newTurns...)
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
			if sendErr := m.Send(ctx, conv, "Sorry, I ran into an error and couldn't finish handling that. Please try again."); sendErr != nil {
				log.Println("failed to deliver error notice:", sendErr)
			}
		}
		return result, err
	}

	// Guarantee delivery: the final answer always reaches the user once the
	// loop completes successfully, regardless of whether the model
	// remembered to call send_message along the way — delivery is a
	// structural consequence of the loop finishing, not something that
	// depends on the model choosing the right tool. Skipped if send_mhowessage
	// already succeeded this turn, so a model that DID use it to reply
	// doesn't get its answer sent twice.
	if err == nil && result.Status == LLMStatusComplete && result.Content != "" {
		alreadyReplied := false
		for _, m := range final[1+len(prior):] {
			if m.Role == "tool" && m.Name == "send_message" && !strings.HasPrefix(m.Content, "error:") {
				alreadyReplied = true
				break
			}
		}
		if !alreadyReplied {
			if m, ok := a.messengers[conv.Platform]; ok {
				if sendErr := m.Send(ctx, conv, result.Content); sendErr != nil {
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
	result, err := a.RunLoop(ctx, msg.Conversation, msg.Payload)
	if err != nil {
		return err
	}

	log.Printf("💬%s: finished handling message from %s/%s: status=%s iterations=%d", a.Name, msg.Conversation.Platform, msg.Conversation.ChatID, result.Status, result.Iteration)

	if result.Status == LLMStatusMaxIteration {
		return fmt.Errorf("message handling hit the %d-iteration cap without completing", a.MaxIterations)
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
// with tools that weren't meant for it. includeUserReply controls whether
// the workflow's own nested toolset gets send_message: true from RunLoop
// (a real conversation, where replying to a human is legitimate), false
// from runDelegatedTask (a workflow triggered mid-delegation has no user to
// message directly, same reasoning as the delegated call itself).
func (a *Agent) workflowToolSpecs(includeUserReply bool) []*tools.ToolSpec {
	specs := make([]*tools.ToolSpec, 0, len(a.Workflows))
	for _, wf := range a.Workflows {
		specs = append(specs, a.workflowToolSpec(wf, includeUserReply))
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

func (a *Agent) runWorkflow(ctx context.Context, wf *workflow.Workflow, includeUserReply bool, userTrigger string) (*LoopResult, error) {
	var depth int
	var withinLimit bool
	ctx, depth, withinLimit = incrementDelegationDepth(ctx)
	if !withinLimit {
		return nil, fmt.Errorf("workflow %q: delegation depth exceeded (%d > %d) — likely a delegation cycle across workflows", wf.Name, depth, maxDelegationDepth)
	}

	messages := []llm.Message{
		{Role: "system", Content: wf.SystemPrompt + " " + loopProtocolInstructions},
		{Role: "user", Content: userTrigger},
	}
	base := a.Tools
	if includeUserReply {
		base = a.baseTools()
	}
	toolset := mergeTools(base, toolMapValues(wf.Tools))
	if wf.AllowDelegation {
		toolset = mergeTools(toolset, a.delegateToolSpecs())
	}
	toolset = filterToolsForPlatform(toolset, a.resolvePlatform(ctx))

	maxIter := wf.Iteration
	if maxIter <= 0 {
		maxIter = a.MaxIterations
	}
	result, _, err := a.runLoopFrom(ctx, messages, toolset, maxIter)
	return result, err
}

// workflowToolSpec wraps a single workflow as a tool. Invoking it runs a
// nested loop (via runWorkflow) scoped to that workflow's own system
// prompt and tools (plus the agent's own tools, and send_message too when
// includeUserReply is true), plus delegate_to_<sibling> tools when
// wf.AllowDelegation is set — off by default, so most workflows still see
// only their own tools, and runWorkflow's depth guard bounds how far any
// resulting delegation chain can actually go.
func (a *Agent) workflowToolSpec(wf *workflow.Workflow, includeUserReply bool) *tools.ToolSpec {
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

			result, err := a.runWorkflow(ctx, wf, includeUserReply, userTrigger)
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
			result, err := target.runDelegatedTask(ctx, input.Task)
			if err != nil {
				return nil, err
			}
			log.Printf("🤝%s: %s finished (%s): %s", a.Name, target.Name, result.Status, result.Content)
			return fmt.Sprintf("%s finished (%s): %s", target.Name, result.Status, result.Content), nil
		}).Build()
}

// runDelegatedTask is the agent-to-agent counterpart to RunLoop's
// agent-to-user path — kept as a genuinely separate method rather than a
// flagged variant of RunLoop, so the distinction is visible in the type
// signature instead of hidden in context. Differences from RunLoop, each
// structural rather than a withheld/flagged behavior:
//   - No ConversationRef parameter: delegation isn't a human conversation,
//     so it doesn't borrow that type to pretend it is.
//   - No memory: a delegated call is a stateless subroutine invocation —
//     each one starts fresh, with no continuity across separate delegated
//     calls even within the same outer conversation.
//   - Its toolset is baseTools() + workflowToolSpecs(false) — everything a
//     normal conversational run gets, including send_message and any
//     messenger tools (add_reaction, ...), routed through whatever
//     ConversationRef the delegating call inherited via ctx — minus
//     delegateToolSpecs(), so no further delegation, preventing a cycle.
func (a *Agent) runDelegatedTask(ctx context.Context, task string) (*LoopResult, error) {
	var depth int
	var withinLimit bool
	ctx, depth, withinLimit = incrementDelegationDepth(ctx)
	if !withinLimit {
		return nil, fmt.Errorf("%s: delegation depth exceeded (%d > %d) — likely a delegation cycle across workflows", a.Name, depth, maxDelegationDepth)
	}

	systemContent := a.systemPrompt() +
		"\n\nThis task was delegated to you by another agent, not requested directly by a human. Always call end_loop with your final_message when done — it is relayed back to the delegating agent automatically, and is the one guaranteed way your answer gets through. If send_message and/or a reaction tool (e.g. add_reaction) are available, you may also use them on the conversation/message that triggered this delegation, as a direct note to the user alongside your final_message — not a replacement for it."

	messages := []llm.Message{
		{Role: "system", Content: systemContent},
		{Role: "user", Content: task},
	}
	toolset := mergeTools(a.baseTools(), a.workflowToolSpecs(false))
	toolset = filterToolsForPlatform(toolset, a.resolvePlatform(ctx))

	result, _, err := a.runLoopFrom(ctx, messages, toolset, a.MaxIterations)
	return result, err
}

type LoopResult struct {
	Iteration int       `json:"iteration"`
	Status    LLMStatus `json:"status"`
	Content   string    `json:"content,omitempty"`
}
