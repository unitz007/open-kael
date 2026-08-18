# How Kael Handles a Message

```text
main()
  └─► runtime.NewRuntime()
        └─► RegisterAgent(agent)   # wires shared EventBus + AgentDirectory into each agent
        └─► Launch(ctx)
              └─► for each agent: go agent.Start(ctx)
                    └─► inbox listener (handles queued messages one at a time, panic-recovered per message)
                    └─► one goroutine per registered Messenger, listening for inbound messages (panic-recovered)
                    └─► cron jobs scheduled for CronTriggerType workflows (panic-recovered)
                    └─► webhook routes registered for WebhookTriggerType workflows (panic-recovered)

Inbound message (any Messenger)
  └─► Messenger.Listen decodes it → Agent.EnqueueMessage(conv, text)
        └─► handleMessage (panic-recovered)
              └─► RunLoop(ctx, conv, text)
                    └─► load this agent's memory (partitioned per Agent.SetMemoryKeyFunc — see Memory)
                    └─► build toolset: agent's own tools (+ send_message if a messenger is
                        registered) + every workflow as a tool + every sibling agent as a
                        delegate_to_<id> tool
                    └─► loop (max Agent.MaxIterations, 6 by default):
                          LLM.Call(messages, tools) — tool_choice is always "required"
                          execute whatever tool was called, via callTool — which blocks on
                          human approval first for any tool with RequiresApproval
                          repeat until end_loop is called
                    └─► persist new turns back to memory
                    └─► deliver the final answer via the messenger, unless send_message
                        already handled it during the run
```

A cron- or webhook-triggered workflow runs the same underlying loop via `runWorkflow` instead of `RunLoop` — see [Workflows & Triggers](../guide/workflows.md) and [System Architecture](../architecture/system.md) for the full breakdown of all three entry points into the loop.
