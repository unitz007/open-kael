package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func TestFollowUps_KeptWithTheConversation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	key := domain.ConversationKey{AgentID: "a1", IdentityID: "bot", ChatID: "c1", UserID: "p1"}
	if skills, at := s.LastSkills(ctx, key); skills != nil || !at.IsZero() {
		t.Fatalf("nothing yet: %v %v", skills, at)
	}
	s.SetLastSkills(ctx, key, []string{"its_apps"})
	s.SetLastSkills(ctx, key, nil) // nothing matched: what it was doing stays
	skills, at := s.LastSkills(ctx, key)
	if len(skills) != 1 || skills[0] != "its_apps" || time.Since(at) > time.Minute {
		t.Fatalf("%v %v", skills, at)
	}
	s.Append(ctx, key, domain.Message{Role: domain.RoleUser, Content: "hi"})
	if skills, _ := s.LastSkills(ctx, key); len(skills) != 1 {
		t.Errorf("a new message keeps it: %v", skills)
	}
	if skills, _ := s.LastSkills(ctx, domain.ConversationKey{AgentID: "a1", IdentityID: "bot", ChatID: "c2"}); skills != nil {
		t.Errorf("another chat: %v", skills)
	}
}
