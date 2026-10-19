package runtime

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// Activity: telling the creator how an agent is doing, as counts (see domain.ActivityRecorder).
// Nothing recorded here is what anyone wrote or who they are. Recording never delays or fails a
// reply: it happens on its own goroutine, and a failure is only logged.

// TopicLabeler says in a few plain words what someone asked for ("book a flight"), without any
// names, numbers or details. It is used only to say what people asked an agent for that it
// couldn't do, and only the label is kept.
type TopicLabeler func(ctx context.Context, request string) string

const (
	failureUnavailable = "unavailable" // the answering service could not be reached
	failureProblem     = "problem"     // anything else
)

// SetActivityRecorder turns on activity counting.
func (h *Host) SetActivityRecorder(r domain.ActivityRecorder) { h.activity = r }

// SetTopicLabeler sets how the topic of something an agent couldn't do is worded. Without one the
// event is still counted, with no topic.
func (h *Host) SetTopicLabeler(f TopicLabeler) { h.topicLabeler = f }

// record runs f off the reply's path.
func (h *Host) record(f func(ctx context.Context)) {
	if h.activity == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("runtime: activity: %v", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		f(ctx)
	}()
}

// notePersonActive counts a person who wrote to an agent. Only people who are signed in count, and
// only a stable identifier of them is passed on (the recorder keeps a one-way code of it).
func (h *Host) notePersonActive(hosted *HostedAgent, conv domain.ConversationRef) {
	if h.activity == nil || conv.UserID == "" {
		return
	}
	agentID, person := hosted.Agent.ID, conv.UserID
	h.record(func(ctx context.Context) { h.activity.PersonActive(ctx, agentID, person) })
}

func (h *Host) noteEvent(hosted *HostedAgent, kind domain.ActivityKind, detail string) {
	if h.activity == nil {
		return
	}
	agentID := hosted.Agent.ID
	h.record(func(ctx context.Context) { h.activity.ActivityEvent(ctx, agentID, kind, detail) })
}

// noteCouldNot counts a request the agent could not do. The request itself is only ever seen by
// the labeler, which turns it into a few plain words; the words are what is kept.
func (h *Host) noteCouldNot(ctx context.Context, hosted *HostedAgent, request string) {
	if h.activity == nil {
		return
	}
	agentID, labeler := hosted.Agent.ID, h.topicLabeler
	h.record(func(ctx context.Context) {
		topic := ""
		if labeler != nil {
			topic = labeler(ctx, request)
		}
		h.activity.ActivityEvent(ctx, agentID, domain.ActivityUnsupported, topic)
	})
}

// failureDetail says in one of two plain categories why a reply did not go through.
func failureDetail(err error) string {
	if err != nil {
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "call llm") || strings.Contains(m, "deadline exceeded") || strings.Contains(m, "no llm") {
			return failureUnavailable
		}
	}
	return failureProblem
}
