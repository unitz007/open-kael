# Kael Platform — Technical Documentation

## Purpose

This module is the runtime an agent runs on: a `Runtime` holding an agent registry and a shared event bus, an `Agent` running a tool-calling loop against any OpenAI-compatible chat-completions API, workflows that get exposed as callable tools, agent-to-agent delegation, pluggable messaging/memory/identity, and an OpenAI-compatible LLM client. It has no opinion about which agents you build, which bot platforms they talk to beyond the two shipped adapters, or which external APIs their tools call — that's application code, living outside this module and importing it.

---

## System Architecture

### `agent/agent.go` — the core

```go
type Agent struct {
	Id, Name, Description, OwnerPrompt, IdentityPrompt string
	Workflows  []*workflow.Workflow
	Model      string
	LLM        llm.LLM
	Tools      []*tools.ToolSpec
	eventBus   EventPublisher                 // SetEventBus
	memory     Memory                         // set in NewAgent
	inBox      *MessageQueue                  // set in NewAgent
	human      *human.Human                   // unused
	messengers map[string]messaging.Messenger // AddMessenger
	directory  AgentDirectory                 // SetDirectory
	identities map[string]identity.Identity   // IdentifyAs
}
```

`NewAgent(id, name, description, identityPrompt string, llm llm.LLM) *Agent` builds an agent with `end_loop` built in, persistent file-backed memory (`newAgentMemory`, falling back to in-memory on any filesystem error), and a 32-slot inbox. Composition happens afterward via `AddTool`, `AddWorkflow`, `AddMessenger`, `IdentifyAs`, `SetEventBus`, `SetDirectory` — `SetEventBus`/`SetDirectory` are normally called by `Runtime.RegisterAgent`, not by application code directly.

#### The loop — `runLoopFrom`

The shared engine behind every entry point (`RunLoop`, a workflow's nested run, and a delegated call). Given a starting transcript and a toolset, it loops up to `maxIterations` (6) times:

1. Threads `a.identities` onto `ctx` via `identity.WithIdentities` — done here, at the top, rather than per-entry-point, so agent-level, nested-workflow, and delegated calls all inherit it, and a delegated call resolves against the *target* agent's identities once `runDelegatedTask` calls back into this function on the target.
2. Calls `a.LLM.Call(messages, toolset)`.
3. **No tool calls at all** → not accepted as a final answer, even though every request sets `tool_choice: "required"` (a provider can still ignore it). Silently accepting freeform content here is how an ungrounded answer — invented rather than fetched via a real tool — would sail through unchecked. The response is appended as an assistant turn, a corrective user-role nudge is appended, and the iteration is consumed rather than the loop ending. The nudge and every skipped answer get logged with the response's real `finish_reason` and `reasoning` fields, since a reasoning-capable model has been observed silently dropping the tool-call handoff entirely (see `examples/llm/openai`).
4. **More than `maxToolCallsPerResponse` (10) tool calls in one response** → discarded wholesale, nothing in the batch executes. This is a decoding glitch some models fall into (dozens of copies of the same call in one shot), not real multi-tool intent — same "consume an iteration, nudge, try again" shape as above.
5. Each remaining tool call is looked up by name; an unrecognized name gets an `error: unknown tool` result. A **repeat guard** (keyed by `name + "\x00" + arguments`) blocks a tool already called once with the same arguments from firing again — except `end_loop`, which is exempt. The guard only marks a call "used" on success, so a transient failure can be retried.
6. `end_loop` is handled specially: its handler returns `endLoopResult{Reason, FinalMessage}`, and `FinalMessage` (not `Reason`) becomes `LoopResult.Content` — the only place the model's answer is captured, since `tool_choice: "required"` rules out a plain-content-only response.
7. Any other successful tool result becomes a `tool`-role message (JSON-marshaled if not already a string) and the loop continues.

Hitting `maxIterations` without an `end_loop` call returns `LLMStatusMaxIteration` — loud, not a silently-accepted answer.

#### Two entry points into the loop, not one with a flag

**`RunLoop(ctx, conv, userPrompt) (*LoopResult, error)`** is the agent-to-user path — the only one `handleMessage` calls. It loads prior turns from memory, builds a toolset via `mergeTools(baseTools(), workflowToolSpecs(true), delegateToolSpecs())` — `send_message` (if a messenger's registered), every workflow, and a `delegate_to_<id>` tool per sibling agent — unconditionally, since this method is never used for anything but a real conversation. Once the loop finishes, `RunLoop` delivers the answer itself: `end_loop`'s final message goes to the conversation's messenger automatically unless `send_message` already succeeded during the run (checked by scanning the newly-added transcript turns for a successful `send_message` result), so nothing goes out twice and a reply doesn't depend on the model remembering to call `send_message` along the way.

**`runDelegatedTask(ctx, task) (*LoopResult, error)`** is the agent-to-agent path, called by `delegate_to_<id>`'s handler. It's a genuinely separate method, not `RunLoop` with something disabled — the distinction is visible in the signature, not hidden behind a context flag:

- No `ConversationRef` parameter — delegation isn't a conversation.
- No memory — a delegated call is a stateless subroutine invocation; nothing persists across separate delegated calls, even within the same outer conversation.
- Toolset is `a.Tools + workflowToolSpecs(false)` — never `baseTools()` (so never `send_message`, since there's no human on the other end to send to) and never `delegateToolSpecs()` (so no further delegation, which is what would let two agents' prompts walk each other into a cycle) — structurally, because this method simply never calls the functions that would add them.

A workflow, wherever it's triggered from, runs the same way: a **fresh** transcript scoped to the workflow's own system prompt, nested one level into the same `runLoopFrom` engine, never with access to other workflows or to delegation — nesting never goes past one level in either direction. `workflowToolSpec`'s `includeUserReply bool` parameter controls whether the nested toolset gets `send_message`: `true` from `RunLoop` (replying to a human is legitimate), `false` from `runDelegatedTask` (a workflow triggered mid-delegation has no user to message, same reasoning as the delegated call itself).

#### `baseTools` and messenger gating

```go
func (a *Agent) baseTools() []*tools.ToolSpec {
	if len(a.messengers) == 0 {
		return a.Tools
	}
	return append(append([]*tools.ToolSpec{}, a.Tools...), a.sendMessageTool())
}
```

`send_message` is only ever added once a messenger has actually been registered via `AddMessenger` — an agent with no messenger has nowhere for it to route to, so offering the tool would just invite a guaranteed-to-fail call. Computed fresh on every call (same as `workflowToolSpecs`/`delegateToolSpecs`) since `AddMessenger` can be called any time after `NewAgent`. `runDelegatedTask` deliberately never calls `baseTools`, so a delegated call's toolset structurally never includes `send_message` — not included-then-withheld, just never added.

`sendMessageTool`'s handler resolves where to send via `resolveSendTarget`: the active conversation from `ctx` (via `messaging.ConversationFromContext`) if one's attached — replying to an inbound message — or the first registered messenger's `DefaultConversation()` otherwise, for a proactive send with nothing to reply to (e.g. a cron-triggered workflow, once cron actually fires the loop — see Known Issues).

#### Identity

`identity.Identity` is deliberately bare — `Provider() string`, `ActingAs() string`, `Token(ctx) (string, error)` — no action methods, since there's no common action shape across arbitrary providers (opening a GitHub PR and deploying to AWS share nothing). `Token` is a method rather than a field so an implementation can transparently mint/refresh short-lived credentials.

```go
func (a *Agent) IdentifyAs(id identity.Identity) {
	if a.identities == nil {
		a.identities = make(map[string]identity.Identity)
	}
	a.identities[id.Provider()] = id
}
```

Keyed by `Provider()`, so an agent holds at most one declared identity per external system. A tool resolves whichever identity it needs at call time via `identity.FromContext(ctx, provider)` rather than taking one as a constructor argument — necessary because a workflow's own tools are built before the agent that will eventually own them even exists, so there's nothing to close over at construction time. `runLoopFrom` threading `a.identities` onto `ctx` on every call is what makes this resolve correctly regardless of where the tool was built.

#### System prompt assembly

- **`capabilitiesSummary()`** — a bare bullet list of `baseTools()` descriptions (minus `end_loop`) plus workflow descriptions. Deliberately a **leaf computation**: it never includes delegate tools. If it did, two agents that can delegate to each other would each build the other's description by calling back into their own, recursing forever.
- **`capabilitiesBlock()`** — wraps the summary with framing: describe capabilities strictly in terms of what's listed, don't claim more.
- **`systemPrompt()`** — `IdentityPrompt + capabilitiesBlock()`.
- **`delegationBlock()`** — separate from `capabilitiesSummary` for the recursion reason above. Lists sibling agents as prose, appended only in `RunLoop`. Exists because without it, `capabilitiesBlock`'s "strictly based on tools and workflows listed above" instruction leads the model to disclaim a `delegate_to_*` tool it technically has schema access to but was never told about in prose.
- **`loopProtocolInstructions`** — shared text (appended to every agent's `IdentityPrompt`, and to a workflow's `SystemPrompt` for its nested run) explaining the `end_loop`/`tool_choice` protocol and the no-repeat-calls rule.

#### Delegation

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

`delegateToolSpecs()` builds one such tool per sibling visible through `a.directory.GetAgents()` (excluding self), computed fresh on every `RunLoop` call. `a.directory` is nil unless something (normally `Runtime.RegisterAgent`) called `SetDirectory` — a standalone agent with no directory gets zero delegate tools, same as one with zero workflows gets none of those either. The tool's description embeds the target's full `capabilitiesSummary()`, not just a one-line blurb, so the calling model can judge fitness for itself. Invoking it is synchronous — it blocks until the target's `runDelegatedTask` returns and folds the result straight into the delegator's own transcript.

#### `Start(ctx)`

1. Starts the inbox listener (`a.inBox.Listen`), routing each dequeued message through `handleMessage` → `RunLoop`.
2. Starts one goroutine per registered `Messenger`, calling `m.Listen(ctx, ...)` and funneling inbound messages into `a.EnqueueMessage`.
3. For each workflow with a `CronTriggerType` trigger, schedules a `gocron` job. **The scheduled task only logs and publishes a `workflow.triggered` event — it does not call into the agent's loop.** See Known Issues.
4. Logs `🤖{Name} started successfully` and publishes an `agent.started` event (if an event bus is wired in).

---

### `agent/inbox.go` — `MessageQueue`

A buffered-channel queue (`InBox{ID, Conversation, Payload}`). `Enqueue` blocks if the buffer (32, set in `NewAgent`) is full. `Listen(ctx, handler)` starts a goroutine draining the channel and calling `handler` sequentially — one agent processes messages one at a time, never concurrently. `Listen` can be called more than once for a worker pool (documented, not used by anything in this module).

---

### `runtime/runtime.go`

```go
type Runtime struct {
	agentRegistry []*agent.Agent
	EventBus      *events.EventBus
}
```

- **`RegisterAgent(a)`** — no-ops (with a log line) if an agent with the same `Id` is already registered. Otherwise calls `a.SetEventBus(r.EventBus)` and `a.SetDirectory(r)`, appends to the registry, and publishes `agent.registered`. `*Runtime` satisfies `agent.AgentDirectory` (`GetAgents() []*agent.Agent`) structurally — `agent` never imports `runtime`, avoiding an import cycle; `EventPublisher`/`*events.EventBus` use the same trick.
- **`Launch(ctx)`** — publishes `runtime.started`, starts every registered agent's `Start(ctx)` in its own goroutine (fire-and-forget, no `WaitGroup`), logs `🎯kael started successfully with N agent(s).`, blocks on `ctx.Done()`.
- **`GetAgents()`** / **`FindAgent(id)`** — plain accessors over the registry.

---

### `messaging/` — `Messenger` interface + two implementations

**`messaging.go`** defines the platform-agnostic types:

```go
type ConversationRef struct { Platform, ChatID string }
type InboundMessage struct { Conversation ConversationRef; Text string }

type Messenger interface {
	Platform() string
	Send(ctx context.Context, conv ConversationRef, text string) error
	Listen(ctx context.Context, onMessage func(InboundMessage)) error
	DefaultConversation() ConversationRef
}
```

Plus `WithConversation`/`ConversationFromContext` — context helpers, since a tool handler only ever receives `(ctx, args)` (`tools.HandlerFunc`), so `ctx` is the only channel through which `send_message`'s handler learns which conversation it's replying to.

**`telegram.go`** — `TelegramBot` (`NewTelegramBot()` reads `TELEGRAM_TOKEN`/`TELEGRAM_CHAT_ID`):

- **`Send`** — runs the message through `formatForTelegram` (markdown → Telegram HTML: `goldmark.Convert` plus manual handling for lists, headings, and `<hr>`, since Telegram's HTML mode doesn't support those natively — an unrecognized `<hr>` tag makes Telegram reject the whole message), POSTs to `sendMessage` with `parse_mode=HTML`.
- **`Listen`** — long-polls `getUpdates?offset=N&timeout=30` until `ctx.Done()`, checking the response's own `ok` field — a failed poll (bad token, or a 409 from a second process polling the same bot token) still decodes as valid JSON with an empty `Result`, which would otherwise look identical to "no new messages" and fail silently.

**`slack.go`** — `SlackBot` (`NewSlackBot()` reads `SLACK_APP_TOKEN`/`SLACK_BOT_TOKEN`), Socket Mode:

- **`Send`** — POSTs to `chat.postMessage`.
- **`Listen`** — opens a Socket Mode URL via `apps.connections.open`, then `listenOnce` holds a `gorilla/websocket` connection, acks each envelope, filters out bot/subtype messages, and reconnects on a `disconnect` envelope or a read error.

Only one process should hold a given bot token's connection at a time — Telegram allows a single active `getUpdates` long-poll per token, and a duplicate process just steals or drops messages between the two silently (Telegram's own `ok: false` at least surfaces this in the log; nothing currently prevents running two instances in the first place).

---

### `memory/` — `Memory` interface + two implementations

```go
type Memory interface {
	History(id string) []llm.Message
	Append(id string, messages ...llm.Message)
}
```

- **`memory.go`** — `InMemoryHistory`: a mutex-guarded `map[string][]llm.Message`, process-local, lost on restart.
- **`file.go`** — `FileHistory`: same per-id, trim-to-`maxHistoryMessages`-per-id semantics, backed by a JSON file. `Append` persists the full updated store on every call via write-to-temp-then-`os.Rename` (atomic on the same filesystem, so a crash mid-write can't leave a half-written file), 0600 permissions.

`NewAgent` wires every agent to a `FileHistory` at `data/memory/<id>.json` via `newAgentMemory`, falling back to `InMemoryHistory` if the directory can't be created or the file can't be loaded — a filesystem problem degrades that one agent's memory rather than crashing the runtime.

`id` is an arbitrary string chosen by the caller — `RunLoop`/`runWorkflow`/`runDelegatedTask` all derive it via `Agent.memoryKey`, which delegates to a configurable `messaging.MemoryKeyFunc` (`Agent.SetMemoryKeyFunc`). The default, `messaging.KeyByAgent()`, reproduces the platform's original behavior — one shared key ("owner") for every call regardless of platform/conversation/thread — but isn't the only option: `KeyByConversation`/`KeyByThread` partition by `ConversationRef`/Slack thread, and `KeyByWorkflow` gives each workflow (see `workflow.Workflow`) its own ongoing bucket across every run, including replies traced back to it via a Messenger-specific tagging mechanism (see `messaging.WithWorkflowID`). See `messaging/messaging.go` for the full set of presets and how to compose or write your own.

---

### `workflow/` and `triggers/`

```go
// workflow/workflow.go
type Workflow struct {
	ID, Name, Description, SystemPrompt string
	Iteration int
	Trigger   triggers.Trigger
	Tools     map[string]*tools.ToolSpec
}

// triggers/trigger.go
type TriggerType string
const (
	CronTriggerType    TriggerType = "trigger.cron"
	WebhookTriggerType TriggerType = "trigger.webhook"
	EventTriggerType   TriggerType = "trigger.event"
)
type Trigger struct { Type TriggerType; Value string }
```

Only `CronTriggerType` has any handling in `Agent.Start` (scheduling via `gocron`) — and even that doesn't execute the workflow yet (see Known Issues). `WebhookTriggerType`/`EventTriggerType` are declared but unimplemented; nothing in this module currently produces or consumes them.

---

### `tools/tool.go`

```go
type HandlerFunc func(ctx context.Context, message json.RawMessage) (any, error)

type ToolSpec struct {
	Name, Description string
	Parameters []ToolRequestParameters
	Handler    HandlerFunc
}
```

`ToolSpecBuilder`: `NewToolBuilder(name, description)` → `.Parameter(name, type, description, required)` (repeatable) → `.Handler(fn)` → `.Build()`. `ToolCall` is the parsed-response shape used internally (`{Type, Index, Id, Name, Arguments}`) — distinct from the wire shape (`openai.ToolCallRequest`) used when echoing a tool call back into a replayed assistant message.

Tool arguments arrive **double-encoded** — `args` is a `json.RawMessage` containing a JSON *string*, which itself contains the actual JSON object. Every handler in this codebase (built-in and example) unwraps it the same way:

```go
var raw string
json.Unmarshal(args, &raw)
var input struct{ Param1 string `json:"param1"` }
json.Unmarshal([]byte(raw), &input)
```

---

### `llm/` and `examples/llm/openai/`

**`llm.go`** — the provider-agnostic interface:

```go
type Message struct {
	Role, Content string
	ToolCalls     []tools.ToolCall
	ToolCallID    string
	Name          string
}
type Response struct {
	Status                    string
	Error                     error
	ToolCalls                 []tools.ToolCall
	Content                   string
	FinishReason, Reasoning   string // diagnostic — see runLoopFrom's tool-less-response nudge
}
type LLM interface {
	Call(messages []Message, tools []*tools.ToolSpec) (*Response, error)
}
```

**`examples/llm/openai/`** — an OpenAI-compatible chat-completions implementation (used against OpenRouter in practice):

- **`request.go`** — `NewRequest(model, messages, tools, enableReasoning)` builds the wire payload, always setting `ToolChoice: "required"` and `Reasoning: &ReasoningConfig{Enabled: enableReasoning}`. Each `llm.Message`'s `ToolCalls` gets converted to the **nested** wire shape (`{id, type, function: {name, arguments}}`) — sending the flat internal shape here is silently accepted by the API's schema but leaves the message with no `tool_calls` the API can recognize, so a following `role: "tool"` message gets rejected as orphaned.
- **`client.go`** — `NewClient(model, enableReasoning)` reads `LLM_API_KEY`/`LLM_BASE_URL` from the environment (`os.Exit(1)` if either is missing), uses a package-level `httpClient` with a 120s timeout (not `http.DefaultClient`, which has none — a hung provider would otherwise block `Call`, and with it the single inbox-processing goroutine and everything queued behind it, forever with no error). `Call` POSTs to `{baseUrl}/api/v1/chat/completions`; on a non-200 response it reads and includes the actual response body in the returned error.

`enableReasoning`: a reasoning-capable model has been observed writing a completely correct plan into its `reasoning` output — "I need to call `get_secret_code`" — and then ending the response (`finish_reason: "stop"`) without the actual `tool_calls` ever landing. Not truncation, not confusion about what to do — the handoff from reasoning to the structured tool-call output just didn't happen. Passing `false` requests `reasoning: {enabled: false}` and sidesteps it.

---

### `events/events.go`

```go
type EventBus struct {
	mu          sync.RWMutex
	subscribers []chan Event
	buffer      []Event
	maxBuffer   int // 1000
}
```

Simple pub/sub. `Subscribe()` returns a 100-buffered channel; `Publish`/`PublishEvent` appends to a capped buffer and fans out to subscribers non-blockingly (a full subscriber channel just drops the event rather than blocking the publisher). `GetHistory()` returns a snapshot of the buffer. `Agent` publishes `agent.started`/`workflow.triggered`; `Runtime` publishes `runtime.started`/`agent.registered`. Nothing in this module subscribes to the bus itself — it exists for an application (or a future TUI/observability layer) to consume.

---

### `human/human.go`

A two-field placeholder (`Human{Name string}`), referenced by `Agent.human` but never populated or read anywhere.

---

### `examples/`

- **`examples/researchspecialist/agent.go`** — `Agent()` builds an agent with a single `get_secret_code` tool (a fixed lookup value) and **no messenger** — reachable only via `delegate_to_research_specialist` from another agent. Exists to prove delegation end to end without needing any external credentials.
- **`examples/basic/main.go`** — a runnable `main` registering an assistant agent and the research specialist on a `Runtime` and launching it. Needs `LLM_API_KEY`/`LLM_BASE_URL`; add a `Messenger` via `AddMessenger` to actually receive/reply to anything.

---

## Data Flow

### Startup

```
main()
  → runtime.NewRuntime()
  → rt.RegisterAgent(agentA)   // wires EventBus + AgentDirectory
  → rt.RegisterAgent(agentB)
  → rt.Launch(ctx)
      → EventBus.PublishEvent("runtime.started", ...)
      → for each agent: go agent.Start(ctx)
          → inBox.Listen(ctx, handleMessage)
          → for each messenger: go messenger.Listen(ctx, EnqueueMessage)
          → for each cron workflow: schedule gocron job (fires but doesn't execute — see Known Issues)
          → log "🤖{Name} started successfully"
      → log "🎯kael started successfully with N agent(s)."
      → block on ctx.Done()
```

### Inbound message (any Messenger)

```
Messenger.Listen decodes an inbound message
  → onMessage(InboundMessage) → Agent.EnqueueMessage(conv, text)
      → MessageQueue.Listen's goroutine dequeues → handleMessage(msg)
          → ctx := messaging.WithConversation(background, msg.Conversation)
          → RunLoop(ctx, conv, payload)
              → load owner history, build system prompt + toolset
              → runLoopFrom (identities threaded onto ctx; see loop mechanics above)
                  → may call send_message, a workflow tool, or a delegate_to_* tool
                  → terminates via end_loop
              → persist new turns to memory
              → deliver final answer if send_message wasn't already used
          → log finished (status, iterations)
```

### Delegation

```
Agent A's loop calls delegate_to_B(task)
  → log "🤝A: delegating to B: ..."
  → B.runDelegatedTask(ctx, task)
      → B's toolset: B's own tools + B's own workflowToolSpecs(false)
        (no send_message, no delegateToolSpecs — one level of nesting only)
      → B's system prompt notes this is a delegated call, not a human request
      → B's loop runs to completion, returns via end_loop's final_message
  → log "🤝A: B finished (status): content"
  → that string becomes delegate_to_B's tool result, fed back into A's own loop
```

---

## Environment Variables

None of these are read by this module in general — they're specific to whichever pieces an application actually wires up:

| Variable | Read by |
|----------|---------|
| `LLM_API_KEY`, `LLM_BASE_URL` | `examples/llm/openai.NewClient` — `os.Exit(1)` if either is missing |
| `TELEGRAM_TOKEN`, `TELEGRAM_CHAT_ID` | `messaging.NewTelegramBot` |
| `SLACK_APP_TOKEN`, `SLACK_BOT_TOKEN` | `messaging.NewSlackBot` |

An application adding its own tools/identities (a specific external API, a specific GitHub App) is responsible for its own environment variables and validation — this module has no knowledge of those.

---

## Known Issues

| Location | Issue | Severity |
|----------|-------|----------|
| `agent/agent.go` `Start` | Cron-triggered workflow tasks only log + publish an event; they never call into the agent's loop, so scheduled workflows don't actually execute | High |
| `runtime/runtime.go` `Launch` | Agent goroutines are fire-and-forget — no `sync.WaitGroup` for graceful shutdown | Medium |
| Operational | Running two instances against the same bot token/connection silently steals or loses messages — logged (Telegram's `ok` check; Slack reconnects on error) but nothing prevents it | Medium |
| `human/human.go` | `Agent.human` is set nowhere and read nowhere — dead field | Low |
| `events/events.go` | Nothing in this module subscribes to the event bus itself | Low |
| Testing | The loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap) have been manually/throwaway-tested during development but have no permanent test coverage | Low |

---

## Extension Points

### Adding a tool

```go
tool := tools.NewToolBuilder("my_tool", "Does something").
	Parameter("param1", "string", "Description", true).
	Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
		// unwrap args as shown above
		return result, nil
	}).
	Build()

a.AddTool(tool) // agent-level, or add to a Workflow.Tools map for a workflow-scoped one
```

### Adding an agent

```go
func MyAgent() *agent.Agent {
	a := agent.NewAgent("my_agent", "My Agent", "description", "You are...", openai.NewClient("model-name", false))
	a.AddTool(myTool)
	a.AddWorkflow(&myWorkflow)
	a.AddMessenger(messaging.NewTelegramBot()) // only if it should be directly reachable
	a.IdentifyAs(myIdentity)                   // only if it needs to act on an external system
	return a
}
```

Register with `rt.RegisterAgent(MyAgent())` — that's what makes it visible to (and able to delegate with) the other agents on the same `Runtime`.

### Adding a Messenger

```go
type MyBot struct{ /* ... */ }

func (b *MyBot) Platform() string { return "mychannel" }
func (b *MyBot) Send(ctx context.Context, conv messaging.ConversationRef, text string) error { /* ... */ }
func (b *MyBot) Listen(ctx context.Context, onMessage func(messaging.InboundMessage)) error { /* ... */ }
func (b *MyBot) DefaultConversation() messaging.ConversationRef { /* ... */ }
```

Anything satisfying `messaging.Messenger` works with `AddMessenger` — no other code needs to change. Only one agent (really, one process) should own a given underlying connection/bot token — there's no shared-listener/router mechanism for multiple agents to field messages from one channel.

### Adding an Identity

```go
type MyIdentity struct{ /* credentials */ }

func (i *MyIdentity) Provider() string { return "myservice" }
func (i *MyIdentity) ActingAs() string { return "the-account-name" }
func (i *MyIdentity) Token(ctx context.Context) (string, error) { /* mint/refresh, return current token */ }
```

Register with `agent.IdentifyAs(myIdentity)`; a tool resolves it with `identity.FromContext(ctx, "myservice")`.

---

## File Map

| File | Responsibility |
|------|----------------|
| `agent/agent.go` | `Agent` type, the loop, `RunLoop`/`runDelegatedTask`, workflows-as-tools, delegation, identity |
| `agent/inbox.go` | `MessageQueue` — buffered inbound message queue |
| `runtime/runtime.go` | Agent registry, shared event bus, `Launch` |
| `messaging/messaging.go` | `Messenger` interface, `ConversationRef`, context helpers |
| `messaging/telegram.go` | Telegram `Send`/`Listen` + markdown→HTML formatting |
| `messaging/slack.go` | Slack Socket Mode `Send`/`Listen` |
| `identity/identity.go` | `Identity` interface, context helpers |
| `memory/memory.go` | `Memory` interface, `InMemoryHistory` |
| `memory/file.go` | `FileHistory` — JSON-file-backed, atomic writes |
| `workflow/workflow.go` | `Workflow` struct |
| `triggers/trigger.go` | `TriggerType`, `Trigger` |
| `tools/tool.go` | `ToolSpec`, `ToolSpecBuilder` |
| `llm/llm.go` | Provider-agnostic `LLM` interface, `Message`/`Response` |
| `examples/llm/openai/` | OpenAI-compatible chat-completions client |
| `events/events.go` | `EventBus` |
| `human/human.go` | Placeholder type, currently unused |
| `examples/researchspecialist/agent.go` | Delegation-only demo agent |
| `examples/basic/main.go` | Minimal runnable two-agent example |

---

## Suggested Improvements

- [ ] Make cron-triggered workflows actually execute (`Start`'s scheduled task currently only logs and publishes an event)
- [ ] Add `sync.WaitGroup` to `Runtime.Launch` for graceful shutdown
- [ ] Add tests for the tool-calling loop's guard rails (repeat-blocking, tool-less nudge, runaway-batch cap)
- [ ] A router/addressing pattern for multiple agents to share one messenger channel (currently each agent needs its own bot/token to be directly reachable)
