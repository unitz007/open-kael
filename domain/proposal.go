package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A proposal is a message from an agent that carries choices the person can tap instead of typing, such
// as the best ways to resolve a clash in their calendar. It is saved, so a tap works an hour later or
// after the server has restarted. Tapping is exactly as if the person had typed the sentence behind the
// button: it goes through everything typing would, so a tap can never do what typing could not.

// ProposalChoice is one button.
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

// Button is one tappable button under a message.
type Button struct {
	Label string
	// Data comes back to the host, opaque, when the button is tapped.
	Data string
}

// ButtonAttacher is a messenger that can put buttons under a message it has already sent.
type ButtonAttacher interface {
	AttachButtons(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, messageID string, buttons []Button) error
}
