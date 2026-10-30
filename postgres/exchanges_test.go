package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func TestExchanges_KeptAndRewound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := domain.ConversationKey{AgentID: "a1", IdentityID: "bot", ChatID: "c1", UserID: "p1"}
	s.Append(ctx, key, domain.Message{Role: domain.RoleUser, Content: "hi"}, domain.Message{Role: domain.RoleAssistant, Content: "hello"})
	s.Append(ctx, key, domain.Message{Role: domain.RoleUser, Content: "rain in London?"}, domain.Message{Role: domain.RoleAssistant, Content: "no rain"})
	if _, ok := s.LastExchange(ctx, key); ok {
		t.Fatal("nothing kept yet")
	}
	at := time.Now().Truncate(time.Second)
	s.SetLastExchange(ctx, key, domain.LastExchange{MessageID: "m2", Text: "rain in London?", At: at, ReplyID: "r2", Changed: []string{"Archive Message"}})
	e, ok := s.LastExchange(ctx, key)
	if !ok || e.MessageID != "m2" || e.ReplyID != "r2" || len(e.Changed) != 1 || !e.At.Equal(at) {
		t.Fatalf("%+v %v", e, ok)
	}
	if err := s.RewindLastExchange(ctx, key); err != nil {
		t.Fatal(err)
	}
	h := s.History(ctx, key)
	if len(h) != 2 || h[0].Content != "hi" || h[1].Content != "hello" {
		t.Errorf("only the last exchange is forgotten: %+v", h)
	}
	if _, ok := s.LastExchange(ctx, key); ok {
		t.Error("and the exchange itself")
	}
}
