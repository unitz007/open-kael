# Data Flow

## Startup

```text
main()
  → runtime.NewRuntime()
  → rt.RegisterAgent(agentA)   // wires EventBus + AgentDirectory
  → rt.RegisterAgent(agentB)
  → rt.Launch(ctx)
      → EventBus.PublishEvent("runtime.started", ...)
      → for each agent: go agent.Start(ctx)
          → inBox.Listen(ctx, handleMessage)
          → for each messenger: go messenger.Listen(ctx, EnqueueMessage)
          → for each cron workflow: schedule gocron job (calls runWorkflow for real on fire, panic-recovered)
          → for each webhook workflow: register its Path() on the shared mux (registerWebhook)
          → log "🤖{Name} started successfully"
      → log "🎯kael started successfully with N agent(s)."
      → block on ctx.Done()
```

## Inbound message (any Messenger)

```text
Messenger.Listen decodes an inbound message
  → onMessage(InboundMessage) → Agent.EnqueueMessage(conv, text)
      → MessageQueue.Listen's goroutine dequeues → handleMessage(msg)
          → ctx := messaging.WithConversation(background, msg.Conversation)
          → RunLoop(ctx, conv, payload)
              → load owner history, build system prompt + toolset
              → runLoopFrom (identities threaded onto ctx)
                  → may call send_message, a workflow tool, or a delegate_to_* tool
                  → terminates via end_loop
              → persist new turns to memory
              → deliver final answer if send_message wasn't already used
          → log finished (status, iterations)
```

## Delegation

```text
Agent A's loop calls delegate_to_B(task)
  → log "🤝A: delegating to B: ..."
  → B.runDelegatedTask(ctx, task)
      → B's toolset: mergeTools(B.defaultTools(), B.messengerTools(), B.workflowToolSpecs(nil))
        (no send_message, no delegateToolSpecs — one level of nesting only)
      → B's system prompt notes this is a delegated call, not a human request
      → B's loop runs to completion, returns via end_loop's final_message
  → log "🤝A: B finished (status): content"
  → that string becomes delegate_to_B's tool result, fed back into A's own loop
```
