package agent

import (
	"context"
	"log"
	"runtime/debug"
	"sync"

	"github.com/unitz007/kael/messaging"
)

// InBox represents an item in the queue.
type InBox struct {
	ID           int
	Conversation messaging.ConversationRef
	Payload      string
	MessageID    string // the platform's own id for this message, if any — see messaging.InboundMessage.MessageID
	ThreadID     string // the thread this message belongs to, if any — see messaging.InboundMessage.ThreadID
	WorkflowID   string // the workflow this message traces back to, if any — see messaging.InboundMessage.WorkflowID
	FromAgent    string // the sibling agent this message came from, if any — see messaging.InboundMessage.FromAgent
}

// MessageQueue is a simple in-memory message queue backed by a buffered channel.
type MessageQueue struct {
	inBox chan InBox
	wg    sync.WaitGroup
}

// NewMessageQueue creates a queue with the given buffer capacity.
// bufferSize controls how many messages can sit in the queue before
// Enqueue starts blocking (backpressure).
func NewMessageQueue(bufferSize int) *MessageQueue {
	return &MessageQueue{
		inBox: make(chan InBox, bufferSize),
	}
}

// Enqueue adds a message to the queue. Blocks if the queue is full.
func (q *MessageQueue) Enqueue(msg InBox) {
	q.inBox <- msg
}

// Close signals that no more messages will be sent.
// Call this only from the producer side, once, after all Enqueue calls.
func (q *MessageQueue) Close() {
	close(q.inBox)
}

// Listen starts a listener goroutine that processes messages with handler.
// It stops when the context is cancelled or the queue channel is closed
// and drained. You can call Listen multiple times to get a worker pool —
// all listeners read from the same channel, so messages are load-balanced
// across them automatically.
func (q *MessageQueue) Listen(ctx context.Context, handler func(box InBox)) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			select {
			case msg, ok := <-q.inBox:
				if !ok {
					return // queue closed and drained
				}
				// A panic while handling one message (e.g. a malformed
				// upstream API response the caller didn't guard against)
				// must not take down every other agent sharing this
				// process — recover, log, and keep the listener alive.
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("agent: recovered from panic handling message %d: %v\n%s", msg.ID, r, debug.Stack())
						}
					}()
					handler(msg)
				}()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Wait blocks until all listener goroutines have finished.
func (q *MessageQueue) Wait() {
	q.wg.Wait()
}
