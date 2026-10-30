package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// A proposal is a message from an agent that carries choices the person can tap instead of typing, such
// as the best ways to resolve a clash in their calendar. It is saved, so a tap works an hour later or
// after the server has restarted. Tapping is exactly as if the person had typed the sentence behind the
// button: it goes through everything typing would, so a tap can never do what typing could not.

// ProposalChoice is one button.
// PlatformAnswerPrefix starts the Say of a choice the platform answers itself (Host.SetChoiceAnswerer): choosing it
// never reaches the agent as something the person said.
const PlatformAnswerPrefix = "kael:"

type ProposalChoice struct {
	// Label is what the button says: a few words.
	Label string `json:"label"`
	// Say is what the person is taken to say when they tap it, in the first person and specific
	// ("Move my dentist appointment to 10:15 tomorrow").
	Say string `json:"say"`
}

// Proposal is a message that was sent with choices.
type Proposal struct {
	ID         string           `json:"id"`
	PersonID   string           `json:"person_id"`
	AgentID    string           `json:"agent_id"`
	IdentityID string           `json:"identity_id"`
	ChatID     string           `json:"chat_id"`
	ThreadID   string           `json:"thread_id,omitempty"`
	MessageID  string           `json:"message_id"` // the message the buttons are on
	Text       string           `json:"text"`       // what the message said
	Choices    []ProposalChoice `json:"choices"`
	CreatedAt  time.Time        `json:"created_at"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Chosen     *int             `json:"chosen,omitempty"` // which button was tapped, once one was
	ChosenAt   *time.Time       `json:"chosen_at,omitempty"`
}

// ProposalCallbackPrefix starts the callback data of a proposal's button: "prop:<id>:<position>".
const ProposalCallbackPrefix = "prop:"

// ProposalData is the callback data for button i of proposal id.
func ProposalData(id string, i int) string {
	return fmt.Sprintf("%s%s:%d", ProposalCallbackPrefix, id, i)
}

// ParseProposalData reads callback data made by ProposalData. ok is false for anything else.
func ParseProposalData(data string) (id string, i int, ok bool) {
	rest, found := strings.CutPrefix(data, ProposalCallbackPrefix)
	if !found {
		return "", 0, false
	}
	cut := strings.LastIndex(rest, ":")
	if cut <= 0 {
		return "", 0, false
	}
	n := 0
	if _, err := fmt.Sscanf(rest[cut+1:], "%d", &n); err != nil || n < 0 || rest[cut+1:] != fmt.Sprint(n) {
		return "", 0, false
	}
	return rest[:cut], n, true
}

const (
	// MinProposalChoices and MaxProposalChoices bound the buttons under one message.
	MinProposalChoices = 2
	MaxProposalChoices = 5
	// MaxChoiceLabelRunes and MaxChoiceSayRunes bound one button.
	MaxChoiceLabelRunes = 40
	MaxChoiceSayRunes   = 300
	// ProposalLifetime is how long the buttons on a message work.
	ProposalLifetime = 24 * time.Hour
)

// ParseProposalChoices reads the choices an agent gave when it finished: a list of {label, say}. It keeps
// only well-formed, distinct ones, and returns nothing at all unless there are enough of them to be a
// real choice, so a half-formed offer never becomes a single lonely button.
func ParseProposalChoices(raw any) []ProposalChoice {
	list, _ := raw.([]any)
	if list == nil {
		if typed, ok := raw.([]map[string]any); ok {
			for _, m := range typed {
				list = append(list, m)
			}
		}
	}
	var out []ProposalChoice
	seen := map[string]bool{}
	for _, item := range list {
		m, _ := item.(map[string]any)
		label, _ := m["label"].(string)
		say, _ := m["say"].(string)
		label, say = strings.Join(strings.Fields(label), " "), strings.TrimSpace(say)
		if label == "" || say == "" || utf8.RuneCountInString(label) > MaxChoiceLabelRunes || utf8.RuneCountInString(say) > MaxChoiceSayRunes {
			continue
		}
		label = withTiming(label, say)
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ProposalChoice{Label: label, Say: say})
	}
	if len(out) < MinProposalChoices {
		return nil
	}
	if len(out) > MaxProposalChoices {
		out = out[:MaxProposalChoices]
	}
	return out
}

// timing finds when a choice would happen, in the words of what it says: "later", "tomorrow at 9", "this evening",
// "every Friday", "at 18:30", "in an hour".
var timing = regexp.MustCompile(`(?i)\b(later( on| today)?|tonight|this (morning|afternoon|evening|week|weekend)|tomorrow( (morning|afternoon|evening|at \d{1,2}(:\d{2})?( ?[ap]m)?))?|next (week|month|monday|tuesday|wednesday|thursday|friday|saturday|sunday)|every (day|morning|evening|week|monday|tuesday|wednesday|thursday|friday|saturday|sunday)|in (an?|\d+) (minute|hour|day|week)s?|at \d{1,2}(:\d{2})?( ?[ap]m| utc)?|on (monday|tuesday|wednesday|thursday|friday|saturday|sunday))\b`)

// saysWhen is a label that already says when in its own way: a clock time, a day, "today", "now".
var saysWhen = regexp.MustCompile(`(?i)\b(\d{1,2}:\d{2}|\d{1,2} ?[ap]m|now|today|tonight|tomorrow|later|morning|afternoon|evening|daily|weekly|monthly|hourly|(mon|tues|wednes|thurs|fri|satur|sun)days?|mon|tue|wed|thu|fri|sat|sun)\b`)

// withTiming keeps a button honest about when: if what it says happens at some time ("Check back on my USD/CAD order
// later") and its label doesn't say when ("Check USD/CAD order"), the time is added to the label, so tapping it can't
// be mistaken for doing it now (a person tapped that, meaning "check it now", 2026-10-27). A label that already says
// when, or has no room for it, is left as it is.
func withTiming(label, say string) string {
	when := timing.FindString(say)
	if when == "" || timing.MatchString(label) || saysWhen.MatchString(label) {
		return label
	}
	longer := strings.TrimRight(label, ".!?") + " " + strings.ToLower(when)
	if utf8.RuneCountInString(longer) > MaxChoiceLabelRunes {
		return label
	}
	return longer
}

// Why a tap did not choose anything.
var (
	// ErrProposalChosen means a button on this message was already tapped.
	ErrProposalChosen = errors.New("proposal: already chosen")
	// ErrProposalExpired means the buttons have stopped working.
	ErrProposalExpired = errors.New("proposal: expired")
)

// ProposalStore keeps proposals.
type ProposalStore interface {
	SaveProposal(ctx context.Context, p *Proposal) error
	GetProposal(ctx context.Context, id string) (*Proposal, error) // ErrNotFound when there is none
	// ChoosePosition records that button i was tapped, once and only once: of any number of taps at the
	// same moment, exactly one gets the proposal back and the rest get ErrProposalChosen. It returns
	// ErrProposalExpired after ExpiresAt and ErrNotFound for an unknown id or position.
	ChooseProposal(ctx context.Context, id string, i int, now time.Time) (*Proposal, error)
	// DeleteExpiredProposals removes proposals expired before the given time and returns how many.
	DeleteExpiredProposals(ctx context.Context, before time.Time) (int, error)
}

// OpenProposalFinder finds the newest choices still open in a chat (not chosen, not run out), so choices a chat app
// sends back as the person's own words (Telegram's reply keyboard) are taken as tapped. ErrNotFound when there are none.
type OpenProposalFinder interface {
	LatestOpenProposal(ctx context.Context, agentID, identityID, chatID string, now time.Time) (*Proposal, error)
}

// ProposalCloser closes choices without one being chosen: the person wrote something else instead, so they are over.
// A store that can lets the host take the buttons off the message too.
type ProposalCloser interface {
	CloseProposal(ctx context.Context, id string, now time.Time) error
}

// ExtraKeyboard is the extra (a []string) a chat app that can show taps above the person's keyboard reads with a
// message: each sends its words as the person's own message. Chat apps that can't ignore it.
const ExtraKeyboard = "keyboard"

// Button is one tappable button under a message.
type Button struct {
	Label string
	// Data comes back to the host, opaque, when the button is tapped.
	Data string
}

// TextButtonAttacher is a messenger that puts buttons under a message by writing the message again with them (Slack:
// the buttons are part of its blocks), so it is given the message's own words. A messenger can be either kind.
type TextButtonAttacher interface {
	AttachButtonsToText(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, messageID, text string, buttons []Button) error
}

// ButtonAttacher is a messenger that can put buttons under a message it has already sent.
type ButtonAttacher interface {
	AttachButtons(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, messageID string, buttons []Button) error
}
