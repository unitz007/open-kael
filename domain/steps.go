package domain

import (
	"context"
	"errors"
	"strings"
)

// Steps: which of its abilities an agent uses while it handles something, and how each went, so its creator can see
// it at work and look back on what it did. Only the ability and how it went are ever passed on: never what went in,
// what came back, or who it was for.

// StepState is where one use of an ability is, or how it ended.
type StepState string

const (
	StepRunning  StepState = "running"
	StepWaiting  StepState = "waiting"  // asking the person first
	StepDone     StepState = "done"     // it did its job
	StepFailed   StepState = "failed"   // it didn't go through
	StepDeclined StepState = "declined" // the person said no, or let the question lapse
	StepSignIn   StepState = "signin"   // the person has to connect their account first
	StepStopped  StepState = "stopped"  // the person wrote something new instead of answering
)

// Ended reports whether the step is over.
func (s StepState) Ended() bool { return s != StepRunning && s != StepWaiting }

// StepObserver is told, as an agent works, which action it is using (by the name the model calls it) and where that
// is. It must not block.
type StepObserver interface {
	Step(action string, state StepState)
}

type stepObserverKey struct{}

// WithStepObserver attaches what is told about each step of this run.
func WithStepObserver(ctx context.Context, o StepObserver) context.Context {
	return context.WithValue(ctx, stepObserverKey{}, o)
}

// StepObserverFromContext returns the run's step observer, if any.
func StepObserverFromContext(ctx context.Context) (StepObserver, bool) {
	o, ok := ctx.Value(stepObserverKey{}).(StepObserver)
	return o, ok && o != nil
}

// noteStep tells the run's observer, when there is one.
func noteStep(ctx context.Context, action string, state StepState) {
	if o, ok := StepObserverFromContext(ctx); ok {
		o.Step(action, state)
	}
}

// StepStateOf says how an action ended from what it returned, read the same way as OutcomeOf.
func StepStateOf(output any, err error) StepState {
	if errors.Is(err, ErrSuperseded) {
		return StepStopped
	}
	if err != nil {
		return StepFailed
	}
	if s, ok := output.(string); ok {
		switch {
		case s == OutputNotApproved:
			return StepDeclined
		case strings.HasPrefix(s, OutputAuthPendingPrefix):
			return StepSignIn
		}
	}
	if done, _, _ := OutcomeOf(output, nil); !done {
		return StepFailed
	}
	return StepDone
}

// LiveStep is one ability an agent is using or has used in what it is handling now (Tool is the ability's id).
type LiveStep struct {
	Tool  string    `json:"tool"`
	State StepState `json:"state"`
}

// LiveTurn is what an agent is handling right now: with which capabilities (ids, once routing has chosen; none while
// it is just talking), the abilities it has used so far, and the chat app it came in on (the identity's id; never who
// wrote).
type LiveTurn struct {
	Capabilities []string   `json:"capabilities"`
	Steps        []LiveStep `json:"steps"`
	Chat         string     `json:"chat,omitempty"`
}

// StepWatcher says, for agents at work, what each is handling right now (its latest piece of work). Watchers of
// WorkWatcher are told when this changes too.
type StepWatcher interface {
	LiveTurns(agentIDs []string) map[string]LiveTurn
}

// ConversationWatcher says, for agents at work, every piece of work each is handling right now, oldest first: an agent
// talks to several people at once.
type ConversationWatcher interface {
	LiveConversations(agentIDs []string) map[string][]LiveTurn
}

// TurnRecord is one message from a person that an agent handled: with which capabilities, the abilities it used and
// how each went, and whether the reply went through. Nothing else.
type TurnRecord struct {
	Capabilities []string
	Steps        []LiveStep
	Failed       bool
}

// TurnRecorder keeps what an agent did with each message, for its creator. A recorder that is not one simply does
// not keep it.
type TurnRecorder interface {
	TurnHandled(ctx context.Context, agentID string, rec TurnRecord)
}

// ActivityTurn is one handled message as the creator's screens see it.
type ActivityTurn struct {
	ID           int64      `json:"id"`
	AgentID      string     `json:"agent_id"`
	At           string     `json:"at"`
	Capabilities []string   `json:"capabilities"`
	Steps        []LiveStep `json:"steps"`
	Failed       bool       `json:"failed"`
}

// ActivityToolUse is how often one ability (Tool is its id) was used over the period, by how it went.
type ActivityToolUse struct {
	Tool     string `json:"tool"`
	Done     int    `json:"done"`
	Failed   int    `json:"failed"`
	Declined int    `json:"declined"`
	SignIn   int    `json:"signin"`
	Stopped  int    `json:"stopped"`
}

// Total is every use, however it went.
func (u ActivityToolUse) Total() int { return u.Done + u.Failed + u.Declined + u.SignIn + u.Stopped }
