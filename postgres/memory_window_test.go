package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A long conversation with a look-up and the platform's note in every exchange: the minion is given the person's words
// and its replies from far enough back, the latest three exchanges whole, and no note from the platform.
func TestHistory_TheWordsOfTheConversationNotItsLookUps(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := domain.ConversationKey{AgentID: "a1", IdentityID: "bot", ChatID: "c1", UserID: "p1"}
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("call%d", i)
		s.Append(ctx, key,
			domain.Message{Role: domain.RoleUser, Content: fmt.Sprintf("q%d", i)},
			domain.Message{Role: domain.RoleSystem, Content: "The person wants advice or information, not a change."},
			domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: id, Name: "get_my_team", Arguments: map[string]any{}}}},
			domain.Message{Role: domain.RoleTool, ToolCallID: id, Name: "get_my_team", Content: `{"players":[]}`},
			domain.Message{Role: domain.RoleAssistant, Content: fmt.Sprintf("a%d", i)},
		)
	}
	h := s.History(ctx, key)
	var got string
	for _, m := range h {
		switch {
		case m.Role == domain.RoleSystem:
			t.Fatalf("a note from the platform came back: %+v", m)
		case m.Role == domain.RoleTool:
			got += "[result " + m.ToolCallID + "]"
		case len(m.ToolCalls) > 0:
			got += "[call " + m.ToolCalls[0].ID + "]"
		default:
			got += m.Content + " "
		}
	}
	want := "q8 a8 q9 a9 q10 [call call10][result call10]a10 q11 [call call11][result call11]a11 q12 [call call12][result call12]a12 "
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// One conversation per person and minion: what they said on the web and in a Slack thread is one conversation, and
// what was last talked about follows them from one chat to the next. Another person's and another minion's stay apart.
func TestHistory_OneConversationPerPersonAndMinion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	web := domain.ConversationKey{AgentID: "a1", IdentityID: "web", ChatID: "v1", UserID: "p1"}
	slack := domain.ConversationKey{AgentID: "a1", IdentityID: "slack", ChatID: "D1", ThreadID: "1700.1", UserID: "p1"}
	s.Append(ctx, web, domain.Message{Role: domain.RoleUser, Content: "show my emails"}, domain.Message{Role: domain.RoleAssistant, Content: "three new"})
	s.Append(ctx, slack, domain.Message{Role: domain.RoleUser, Content: "and the next one?"}, domain.Message{Role: domain.RoleAssistant, Content: "from Sam"})
	s.Append(ctx, domain.ConversationKey{AgentID: "a1", IdentityID: "web", ChatID: "v2", UserID: "p2"}, domain.Message{Role: domain.RoleUser, Content: "someone else"})
	s.Append(ctx, domain.ConversationKey{AgentID: "a2", IdentityID: "web", ChatID: "v1", UserID: "p1"}, domain.Message{Role: domain.RoleUser, Content: "another minion"})

	other := slack
	other.ThreadID = "1700.9" // a new message in the same direct message: its own thread on Slack
	var got []string
	for _, m := range s.History(ctx, other) {
		got = append(got, m.Content)
	}
	if strings.Join(got, "|") != "show my emails|three new|and the next one?|from Sam" {
		t.Fatalf("got %q", got)
	}

	s.SetLastSkills(ctx, web, []string{"email"})
	telegram := domain.ConversationKey{AgentID: "a1", IdentityID: "tg", ChatID: "42", UserID: "p1"}
	if skills, _ := s.LastSkills(ctx, telegram); strings.Join(skills, ",") != "email" {
		t.Fatalf("what was last talked about on the web follows them to Telegram: %v", skills)
	}
	s.SetLastSkills(ctx, slack, []string{"calendar"})
	if skills, _ := s.LastSkills(ctx, telegram); strings.Join(skills, ",") != "calendar" {
		t.Fatalf("the latest of their chats: %v", skills)
	}
	if skills, _ := s.LastSkills(ctx, domain.ConversationKey{AgentID: "a1", IdentityID: "tg", ChatID: "43", UserID: "p2"}); len(skills) != 0 {
		t.Fatalf("never another person's: %v", skills)
	}
}
