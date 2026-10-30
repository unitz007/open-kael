package domain

import (
	"context"
	"errors"
	"time"
)

// ── A person editing a message they sent ─────────────────────────────────────
//
// A chat app that hears an edit hands it on as an InboundMessage with Edited set (MessageID the message, Text its
// new words); the host decides what it means, once for every chat app: still waiting, the new words take its place;
// being answered, that reply is dropped and the new words answered; answered with look-ups only, answered again in
// the old reply's place (ReplyReplacer); answered with a change, nothing is redone or undone: the new words are
// answered with look-ups only and the agent is told what was already done. Only the person's latest message, within
// EditWindow.

// EditWindow is how long after sending it a message's edit is answered.
const EditWindow = 15 * time.Minute

// ErrEdited is why a reply in progress is dropped: the person edited the message it answers.
var ErrEdited = errors.New("the person edited the message being answered")

// LastExchange is a conversation's latest message from the person and the reply to it.
type LastExchange struct {
	MessageID string    // the person's message, as the chat app knows it
	Text      string    // its words
	At        time.Time // when it was answered
	ReplyID   string    // the reply, as the chat app knows it
	Changed   []string  // the plain names of what the reply changed ("Archive Message"), none when it only looked
}

// Exchanges is kept by a Memory that can: each conversation's last exchange, and forgetting it from the history (an
// edit's new answer takes its place).
type Exchanges interface {
	LastExchange(ctx context.Context, key ConversationKey) (LastExchange, bool)
	SetLastExchange(ctx context.Context, key ConversationKey, e LastExchange)
	RewindLastExchange(ctx context.Context, key ConversationKey) error
}

// EditNotes is kept by a Memory that can: whether a person may be told, now, that an edit was left alone (not their
// latest message, or too old). At most once a day per conversation; asking takes the turn.
type EditNotes interface {
	MayNoteIgnoredEdit(ctx context.Context, key ConversationKey) bool
}

// EditNoteEvery is how often a conversation may be told that an edit was left alone.
const EditNoteEvery = 24 * time.Hour

// ReplyReplacer is a chat app that can put a new reply in the place of one it sent (showing it was edited, where it
// shows that).
type ReplyReplacer interface {
	ReplaceReply(ctx context.Context, identity *Identity, connectionRef string, conv ConversationRef, messageID, text string) error
}

// MemberEmailer is a chat app that can tell the email it has for a member who writes ("" for none it may see).
type MemberEmailer interface {
	MemberEmail(ctx context.Context, identity *Identity, senderID string) (string, error)
}

type editCtxKey int

const (
	replacingKey editCtxKey = iota
	lookUpsOnlyKey
	turnHintKey
)

// WithReplacingReply makes a turn's reply take the place of one sent before (ReplacingReply reads it).
func WithReplacingReply(ctx context.Context, messageID string) context.Context {
	if messageID == "" {
		return ctx
	}
	return context.WithValue(ctx, replacingKey, messageID)
}

func ReplacingReply(ctx context.Context) string {
	id, _ := ctx.Value(replacingKey).(string)
	return id
}

// WithLookUpsOnly answers a turn with look-ups only: nothing is changed (IsLookUpsOnly reads it).
func WithLookUpsOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, lookUpsOnlyKey, true)
}

func IsLookUpsOnly(ctx context.Context) bool {
	v, _ := ctx.Value(lookUpsOnlyKey).(bool)
	return v
}

// WithTurnHint tells the agent something about this one turn, after its own instructions (TurnHint reads it).
func WithTurnHint(ctx context.Context, hint string) context.Context {
	if hint == "" {
		return ctx
	}
	return context.WithValue(ctx, turnHintKey, hint)
}

func TurnHint(ctx context.Context) string {
	h, _ := ctx.Value(turnHintKey).(string)
	return h
}
