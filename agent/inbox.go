package agent

import (
	"github.com/unitz007/kael/messaging"
	"context"
	"sync"
)

// InBox represents an item in the queue.
type InBox struct {
	ID           int
	Conversation messaging.ConversationRef
	Payload      string
	MessageID    string // the platform's own id for this message, if any — see messaging.InboundMessage.MessageID
	ThreadID     string // the thread this message belongs to, if any — see messaging.InboundMessage.ThreadID
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
				handler(msg)
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

//func main() {
//queue := NewQueue(10)
//
//ctx, cancel := context.WithCancel(context.Background())
//defer cancel()
//
//// Start one listener. Call queue.Listen(...) again with a different
//// goroutine to get concurrent worker processing of the same queue.
//queue.Listen(ctx, func(msg Message) {
//	fmt.Printf("received message %d: %s\n", msg.ID, msg.Payload)
//})
//
//// Producer: send some messages.
//for i := 1; i <= 5; i++ {
//	queue.Enqueue(Message{ID: i, Payload: fmt.Sprintf("hello %d", i)})
//	time.Sleep(100 * time.Millisecond)
//}
//
//queue.Close() // no more messages coming
//queue.Wait()  // wait for the listener to finish draining
//}
