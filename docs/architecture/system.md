# System Architecture

This module has no opinion about which agents you build, which bot platforms they talk to beyond the two reference adapters, or which external APIs their tools call — that's application code, living outside this module and importing it.

## `agent/agent.go` — the core

```go
type Agent struct {
	Id, Name, Description, OwnerPrompt, IdentityPrompt string
	Workflows  []*workflow.Workflow
	Model      string
	LLMs       []llm.LLM // ordered priority chain — LLMs[0] is the default, the rest are fallbacks
	llmState   []llmProviderState // parallel to LLMs — circuit-breaker cooldown tracking, see callLLM
	Tools      []*tools.ToolSpec
	eventBus   EventPublisher                 // SetEventBus
	memory     Memory                         // set in NewAgent (a bare in-memory default — see Memory)
	inBox      *MessageQueue                  // set in NewAgent
	human      *human.Human                   // unused
	messengers map[string]messaging.Messenger // AddMessenger
	directory  AgentDirectory                 // SetDirectory
	identities map[string]identity.Identity   // IdentifyAs
}
```

`NewAgent(id, name, description, identityPrompt string, llms ...llm.LLM) *Agent` builds an agent with `end_loop` built in, a bare process-local memory default (`newBareMemory()` — see [Memory](../guide/memory.md) for why nothing more persistent ships in this module), and a 32-slot inbox. `llms` is variadic — passing a single provider works as before; passing more than one gives the agent an ordered fallback chain. Composition happens afterward via `AddTool`, `AddWorkflow`, `AddMessenger`, `IdentifyAs`, `SetEventBus`, `SetDirectory` — `SetEventBus`/`SetDirectory` are normally called by `Runtime.RegisterAgent`, not by application code directly.

## The loop — `runLoopFrom`

The shared engine behind every entry point (`RunLoop`, `runWorkflow`, and `runDelegatedTask`). Given a starting transcript and a toolset, it loops up to `maxIterations` (6 by default) times:

1. Threads `a.identities` onto `ctx` via `identity.WithIdentities` — done here, at the top, rather than per-entry-point, so agent-level, nested-workflow, and delegated calls all inherit it, and a delegated call resolves against the *target* agent's identities once `runDelegatedTask` calls back into this function on the target.
2. Calls `a.LLM.Call(messages, toolset)`.
3. **No tool calls at all** → not accepted as a final answer, even though every request sets `tool_choice: "required"` (a provider can still ignore it). Silently accepting freeform content here is how an ungrounded answer — invented rather than fetched via a real tool — would sail through unchecked. The response is appended as an assistant turn, a corrective user-role nudge is appended, and the iteration is consumed rather than the loop ending.
4. **More than `maxToolCallsPerResponse` tool calls in one response** → discarded wholesale, nothing in the batch executes. This is a decoding glitch some models fall into (dozens of copies of the same call in one shot), not real multi-tool intent.
5. Each remaining tool call is looked up by name; an unrecognized name gets an `error: unknown tool` result. A **repeat guard** (keyed by `name + "\x00" + arguments`) blocks a tool already called once with the same arguments from firing again — except `end_loop`, which is exempt. The guard only marks a call "used" on success, so a transient failure can be retried.
6. `end_loop` is handled specially: its handler returns `endLoopResult{Reason, FinalMessage}`, and `FinalMessage` (not `Reason`) becomes `LoopResult.Content` — the only place the model's answer is captured, since `tool_choice: "required"` rules out a plain-content-only response.
7. Any other successful tool result becomes a `tool`-role message (JSON-marshaled if not already a string) and the loop continues.

Hitting `maxIterations` without an `end_loop` call returns `LLMStatusMaxIteration` — loud, not a silently-accepted answer.

## Three entry points into the loop, not one with a flag

**`RunLoop(ctx, conv, userPrompt) (*LoopResult, error)`** is the agent-to-user path — the only one `handleMessage` calls. It loads prior turns from memory (keyed via `a.memoryKey(ctx, conv)`), builds a toolset via `mergeTools(a.baseTools(), a.workflowToolSpecs(a.messagingTools()), a.delegateToolSpecs())` — every workflow and a `delegate_to_<id>` tool per sibling agent, unconditionally, since this method is never used for anything but a real conversation. Once the loop finishes, `RunLoop` delivers the answer itself: `end_loop`'s final message goes to the conversation's messenger automatically unless `send_message` already succeeded during the run, so nothing goes out twice and a reply doesn't depend on the model remembering to call `send_message` along the way. A genuine LLM/infra failure gets a best-effort "I ran into an error" notice sent to the user rather than the conversation just going silent.

**`runWorkflow(ctx, wf, messagingTools, userTrigger) (*LoopResult, error)`** is the cron/webhook-trigger and chat-invoked-as-a-tool path — a **fresh** transcript scoped to the workflow's own system prompt, nested one level into the same `runLoopFrom` engine, never with access to other workflows. Its toolset is `a.workflowToolset(wf, messagingTools)` — `a.defaultTools()` plus whatever `messagingTools` the caller passes in, merged with `wf.Tools`, plus `a.delegateToolSpecs()` only if `wf.AllowDelegation` is set. The `messagingTools` parameter is what lets the same method serve every caller correctly: `RunLoop` and `Start`'s cron/webhook scheduling both pass `a.messagingTools()` (a workflow needs `send_message` to actually deliver anything), while `runDelegatedTask`'s own nested workflow calls pass `nil`.

**`runDelegatedTask(ctx, task) (*LoopResult, error)`** is the agent-to-agent path, called by `delegate_to_<id>`'s handler. It's a genuinely separate method, not `RunLoop` with something disabled:

- No `ConversationRef` parameter — delegation isn't a conversation.
- No memory — a delegated call is a stateless subroutine invocation.
- Toolset is `mergeTools(a.defaultTools(), a.messengerTools(), a.workflowToolSpecs(nil))` — composed directly from what's wanted, never `a.baseTools()` then subtracting `send_message` after the fact. `a.messengerTools()` (platform-contributed tools like Slack's `add_reaction`) is included — a delegate can still react — but `send_message` itself never is, since there's no human on the other end of a delegated call to send *to*. No `delegateToolSpecs()` either, so no further delegation — a hard depth cap (`maxDelegationDepth`) enforces this regardless of any single flag.

## `baseTools`, `defaultTools`, and `messagingTools`

```go
func (a *Agent) baseTools() []*tools.ToolSpec {
	return append(a.defaultTools(), a.messagingTools()...)
}

func (a *Agent) defaultTools() []*tools.ToolSpec {
	out := append([]*tools.ToolSpec{}, a.Tools...)
	out = append(out, a.retrieverToolSpecs()...)
	if a.memory != nil {
		out = append(out, a.getThreadHistoryTool())
	}
	return out
}

func (a *Agent) messagingTools() []*tools.ToolSpec {
	if len(a.messengers) == 0 {
		return nil
	}
	return append([]*tools.ToolSpec{a.sendMessageTool()}, a.messengerTools()...)
}
```

The split matters: `defaultTools()` — `a.Tools` (everything added via `AddTool`, including any `RequiresApproval` tool), retrievers, and thread-history — is genuinely **unconditional**, included everywhere regardless of caller. `messagingTools()` — `send_message` plus any messenger-contributed extras — is the *only* layer that's ever conditionally excluded, because it's the one that requires a live, identity-matched conversation to act through. This is a deliberate split from an earlier design where a single `includeUserReply bool` flag bundled four unrelated tool categories together — the safety property that approval-gated tools stay reachable everywhere was previously emergent, not structural; now it's a direct consequence of which function a tool lives under.

`sendMessageTool`'s handler resolves where to send via `resolveSendTarget`: the active conversation from `ctx` if one's attached — replying to an inbound message — or the first registered messenger's `DefaultConversation()` otherwise, for a proactive send with nothing to reply to (a cron-triggered workflow, for instance).

## Identity

`identity.Identity` is deliberately bare — `Provider() string`, `ActingAs() string`, `Token(ctx) (string, error)` — no action methods, since there's no common action shape across arbitrary providers. `Token` is a method rather than a field so an implementation can transparently mint/refresh short-lived credentials.

```go
func (a *Agent) IdentifyAs(id identity.Identity) {
	if a.identities == nil {
		a.identities = make(map[string]identity.Identity)
	}
	a.identities[id.Provider()] = id
}
```

Keyed by `Provider()`, so an agent holds at most one declared identity per external system. A tool resolves whichever identity it needs at call time via `identity.FromContext(ctx, provider)` rather than taking one as a constructor argument — necessary because a workflow's own tools are built before the agent that will eventually own them even exists. `runLoopFrom` threading `a.identities` onto `ctx` on every call is what makes this resolve correctly regardless of where the tool was built.

## System prompt assembly

- **`capabilitiesSummary()`** — a bare bullet list of `baseTools()` descriptions (minus `end_loop`) plus workflow descriptions. Deliberately a **leaf computation**: it never includes delegate tools. If it did, two agents that can delegate to each other would each build the other's description by calling back into their own, recursing forever.
- **`capabilitiesBlock()`** — wraps the summary with framing: describe capabilities strictly in terms of what's listed, don't claim more.
- **`systemPrompt()`** — `IdentityPrompt + capabilitiesBlock()`.
- **`delegationBlock()`** — separate from `capabilitiesSummary` for the recursion reason above. Lists sibling agents as prose, appended only in `RunLoop`.
- **`loopProtocolInstructions`** — shared text (appended to every agent's `IdentityPrompt`, and to a workflow's `SystemPrompt` for its nested run) explaining the `end_loop`/`tool_choice` protocol and the no-repeat-calls rule.

## Delegation

```go
func (a *Agent) delegateToolSpec(target *Agent) *tools.ToolSpec {
	description := fmt.Sprintf("Delegate a task to %s: %s\nIts capabilities:\n%s",
		target.Name, target.Description, target.capabilitiesSummary())
	return tools.NewToolBuilder("delegate_to_"+target.Id, description).
		Parameter("task", "string", "...", true).
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			result, err := target.runDelegatedTask(ctx, input.Task)
			return fmt.Sprintf("%s finished (%s): %s", target.Name, result.Status, result.Content), nil
		}).Build()
}
```

`delegateToolSpecs()` builds one such tool per sibling visible through `a.directory.GetAgents()` (excluding self), computed fresh on every `RunLoop` call. `a.directory` is nil unless something (normally `Runtime.RegisterAgent`) called `SetDirectory`. The tool's description embeds the target's full `capabilitiesSummary()`, not just a one-line blurb. Invoking it is synchronous — it blocks until the target's `runDelegatedTask` returns and folds the result straight into the delegator's own transcript.

## Approval gating — `callTool`

```go
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
```

`runLoopFrom` calls every tool through `callTool`, never `tool.Handler` directly — this is the single interception point regardless of whether the call originated from `RunLoop`, `runWorkflow`, or `runDelegatedTask`. Resolving via `a.DefaultConversation()` rather than whatever `ConversationRef` happens to be on `ctx` is deliberate and fixes a real bug: during a delegated call, `ctx` carries the *delegating* agent's conversation, not the delegate's — resolving against ambient `ctx` would try to post an approval prompt into a conversation belonging to a different agent's messaging identity. A messenger that doesn't implement `messaging.ApprovalMessenger` fails the call with a clear error rather than silently running the handler unapproved.

## `Start(ctx)`

1. Starts the inbox listener (`a.inBox.Listen`), routing each dequeued message through `handleMessage` → `RunLoop`. Panic-recovered — see [Reliability](#reliability).
2. Starts one goroutine per registered `Messenger`, calling `m.Listen(ctx, ...)` and funneling inbound messages into `a.EnqueueMessage`. Also panic-recovered.
3. For each workflow with a `CronTriggerType` trigger, schedules a `gocron` job whose task calls `a.runWorkflow(...)` directly on fire — a real run, not just an event publish. Also panic-recovered.
4. For each workflow with a `WebhookTriggerType` trigger, registers the `webhook.Source`'s `Path()` on the shared mux (`registerWebhook`) and, on a verified/decoded request, runs `a.runWorkflow(...)` in its own panic-recovered goroutine.
5. Logs `🤖{Name} started successfully` and publishes an `agent.started` event (if an event bus is wired in).

## Reliability

A panic anywhere in agent-supplied logic (a tool handler, an LLM client bug, a malformed upstream response) is recovered and logged — full stack trace, via `recoverFromPanic(agentName, context)` — at every goroutine entry point listed in `Start(ctx)` above, rather than being left to propagate and crash the whole process. This matters specifically because a single binary commonly hosts several independent agents sharing one process — without this, one agent's bad day reboots every other agent's live Slack connections, in-flight cron schedules, and any stateful watch/subscription they hold, not just its own. Recovery is a containment measure, not a fix — the logged stack trace is what makes the actual root cause diagnosable afterward.

## `agent/inbox.go` — `MessageQueue`

A buffered-channel queue (`InBox{ID, Conversation, Payload, MessageID, ThreadID, WorkflowID}`). `Enqueue` blocks if the buffer (32, set in `NewAgent`) is full. `Listen(ctx, handler)` starts a goroutine draining the channel and calling `handler` sequentially — one agent processes messages one at a time, never concurrently. Each `handler(msg)` call is wrapped in its own `recover()` — one malformed message can't kill the listener goroutine. `Listen` can be called more than once for a worker pool (documented, not used by anything in this module).

## `runtime/runtime.go`

```go
type Runtime struct {
	agentRegistry []*agent.Agent
	EventBus      *events.EventBus
}
```

- **`RegisterAgent(a)`** — no-ops (with a log line) if an agent with the same `Id` is already registered. Otherwise calls `a.SetEventBus(r.EventBus)` and `a.SetDirectory(r)`, appends to the registry, and publishes `agent.registered`. `*Runtime` satisfies `agent.AgentDirectory` structurally — `agent` never imports `runtime`, avoiding an import cycle.
- **`Launch(ctx)`** — publishes `runtime.started`, starts every registered agent's `Start(ctx)` in its own goroutine (fire-and-forget, no `WaitGroup`), logs `🎯kael started successfully with N agent(s).`, blocks on `ctx.Done()`.
- **`GetAgents()`** / **`FindAgent(id)`** — plain accessors over the registry.
