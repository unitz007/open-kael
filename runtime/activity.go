package runtime

import (
	"context"
	"errors"
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
	failureCredit      = "credit"      // the answering service has no credit left
	failureUnavailable = "unavailable" // the answering service could not be reached
	failureProblem     = "problem"     // anything else
)

// unavailableText is what a person is told when the answering service can't answer: the truth
// without the reason, which is not theirs to worry about and nothing they can act on.
const unavailableText = "I'm temporarily unavailable right now. Please try again in a little while."

// failureText says what went wrong to the person who asked.
func failureText(err error) string {
	if failureDetail(err) != failureProblem { // the answering service, by type or by how the loop words it
		return unavailableText
	}
	return "Sorry, I ran into an error and couldn't finish handling that. Please try again."
}

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

// noteMessage counts a message the agent handled, by the minute. Only the count is kept.
func (h *Host) noteMessage(hosted *HostedAgent) {
	if h.activity == nil {
		return
	}
	agentID := hosted.Agent.ID
	h.record(func(ctx context.Context) { h.activity.MessageHandled(ctx, agentID) })
}

// noteUses counts what a person's message was handled with: these capabilities (by name, as routing picked them), or
// none. Only messages from a person count: an alert or repeating task is not someone using the agent.
func (h *Host) noteUses(ctx context.Context, hosted *HostedAgent, skillNames []string) {
	var ids []string
	for _, name := range skillNames {
		for _, s := range skillsFor(ctx, hosted) {
			if s.Name == name {
				ids = append(ids, s.ID)
				break
			}
		}
	}
	if steps, ok := stepsOf(ctx); ok {
		steps.setCapabilities(ids) // what it is working on, for its creator's screen
	}
	ur, ok := h.activity.(domain.UseRecorder)
	if !ok || domain.IsUnattended(ctx) {
		return
	}
	agentID := hosted.Agent.ID
	h.record(func(ctx context.Context) { ur.CapabilitiesUsed(ctx, agentID, ids) })
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
	if errors.Is(err, domain.ErrLLMOutOfCredit) {
		return failureCredit
	}
	if errors.Is(err, domain.ErrLLMUnavailable) {
		return failureUnavailable
	}
	if err != nil {
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "call llm") || strings.Contains(m, "deadline exceeded") || strings.Contains(m, "no llm") {
			return failureUnavailable
		}
	}
	return failureProblem
}
