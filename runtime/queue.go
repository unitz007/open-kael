package runtime

import (
	"context"
	"time"

	"github.com/unitz007/kael/agent"
	"github.com/unitz007/kael/messaging"
)

// PendingTask is one delegated task that couldn't be delivered because its
// target was unreachable when it was requested — everything needed to
// re-dispatch it later, and to route the eventual result back to wherever
// it came from.
type PendingTask struct {
	ID        string
	TargetID  string                    // the DelegateTarget this was meant for
	OriginID  string                    // the agent that requested it, from messaging.DelegatorFromContext — empty if there was none
	Task      string
	Ref       messaging.ConversationRef // zero value if there was no live conversation when this was queued
	ThreadID  string
	MessageID string
	QueuedAt  time.Time
}

// TaskQueue defers a task meant for a DelegateTarget that's currently
// unreachable until it reconnects. Ships no implementation here — same
// stance agent.TriggerState/agent.AgentLoop/agent.DelegateTarget already
// take: kael-platform defines the contract, the consuming app supplies a
// durable store.
type TaskQueue interface {
	Enqueue(ctx context.Context, task PendingTask) error
	// Drain returns, and removes, every task queued for targetID — called
	// once a previously-unreachable target becomes reachable again.
	Drain(ctx context.Context, targetID string) ([]PendingTask, error)
}

// queuedDelegate implements agent.DelegateTarget for a remote agent id this
// Runtime has seen announce itself at least once (see Runtime.knownRemotes)
// but that isn't reachable through any currently-connected peer right now.
// Unlike remoteDelegate, RunDelegatedTask here never dispatches anything —
// it only enqueues, so delegate_to_<id> stays offered (and keeps working,
// just deferred) instead of vanishing from a caller's toolset the moment
// the peer goes offline.
type queuedDelegate struct {
	info  PeerInfo
	queue TaskQueue
}

func (q *queuedDelegate) DelegateID() string           { return q.info.AgentID }
func (q *queuedDelegate) DelegateName() string         { return q.info.AgentName }
func (q *queuedDelegate) DelegateDescription() string  { return q.info.AgentDescription }
func (q *queuedDelegate) DelegateCapabilities() string { return q.info.AgentCapabilities }

// RunDelegatedTask reads whatever conversation/thread/delegator context the
// calling agent's own message_agent tool already set on ctx (WithDelegator,
// WithConversation/WithThreadID/WithMessageID — see agent.go's
// messageAgentTool and RunLoop/handleMessage) so nothing new needs to be
// plumbed through just to support queuing — it's already there for any
// live-conversation delegation, and simply absent (zero values) for a
// cron/webhook-triggered one, exactly like every other context read this
// codebase already does defensively.
func (q *queuedDelegate) RunDelegatedTask(ctx context.Context, task string) (*agent.LoopResult, error) {
	originID, _ := messaging.DelegatorFromContext(ctx)
	conv, _ := messaging.ConversationFromContext(ctx)
	threadID, _ := messaging.ThreadIDFromContext(ctx)
	messageID, _ := messaging.MessageIDFromContext(ctx)

	pending := PendingTask{
		ID:        newTaskID(q.info.AgentID),
		TargetID:  q.info.AgentID,
		OriginID:  originID,
		Task:      task,
		Ref:       conv,
		ThreadID:  threadID,
		MessageID: messageID,
		QueuedAt:  time.Now(),
	}
	if err := q.queue.Enqueue(ctx, pending); err != nil {
		return nil, err
	}
	return &agent.LoopResult{
		Status:   agent.LLMStatusComplete,
		Content:  "Queued for " + q.info.AgentName + " — will run once it reconnects.",
		Deferred: true,
	}, nil
}
