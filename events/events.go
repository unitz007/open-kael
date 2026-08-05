package events

import (
	"sync"
	"time"
)

type EventType string

const (
	EventRuntimeStarted     EventType = "runtime.started"
	EventAgentRegistered    EventType = "agent.registered"
	EventAgentStarted       EventType = "agent.started"
	EventWorkflowTriggered  EventType = "workflow.triggered"
	EventWorkflowCompleted  EventType = "workflow.completed"
	EventWorkflowFailed     EventType = "workflow.failed"
	EventToolCallStarted    EventType = "tool.call.started"
	EventToolCallCompleted  EventType = "tool.call.completed"
	EventToolCallFailed     EventType = "tool.call.failed"
	EventLLMRequestStarted  EventType = "llm.request.started"
	EventLLMRequestComplete EventType = "llm.request.completed"
	EventLLMRequestFailed   EventType = "llm.request.failed"
)

type Event struct {
	Type      EventType
	Timestamp time.Time
	AgentName string
	Workflow  string
	Message   string
	Error     error
	Data      map[string]interface{}
}

type EventBus struct {
	mu          sync.RWMutex
	subscribers []chan Event
	buffer      []Event
	maxBuffer   int
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make([]chan Event, 0),
		buffer:      make([]Event, 0),
		maxBuffer:   1000,
	}
}

func (eb *EventBus) Subscribe() chan Event {
	eb.mu.Lock()
	defer eb.mu.Unlock()

	ch := make(chan Event, 100)
	eb.subscribers = append(eb.subscribers, ch)
	return ch
}

func (eb *EventBus) Publish(event Event) {
	event.Timestamp = time.Now()

	eb.mu.Lock()
	// Add to buffer
	eb.buffer = append(eb.buffer, event)
	if len(eb.buffer) > eb.maxBuffer {
		eb.buffer = eb.buffer[len(eb.buffer)-eb.maxBuffer:]
	}
	eb.mu.Unlock()

	// Publish to subscribers
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	for _, ch := range eb.subscribers {
		select {
		case ch <- event:
		default:
			// Skip if channel is full
		}
	}
}

func (eb *EventBus) PublishEvent(eventType string, agentName, workflow, message string, err error, data map[string]interface{}) {
	eb.Publish(Event{
		Type:      EventType(eventType),
		AgentName: agentName,
		Workflow:  workflow,
		Message:   message,
		Error:     err,
		Data:      data,
	})
}

func (eb *EventBus) GetHistory() []Event {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	history := make([]Event, len(eb.buffer))
	copy(history, eb.buffer)
	return history
}
