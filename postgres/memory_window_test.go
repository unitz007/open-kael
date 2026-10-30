package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

// A long conversation with a look-up and the platform's note in every exchange: the minion is given the person's words
// and its replies from far enough back, the latest exchange whole, and no note from the platform.
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
	want := "q8 a8 q9 a9 q10 a10 q11 a11 q12 [call call12][result call12]a12 "
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
