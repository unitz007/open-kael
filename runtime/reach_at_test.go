package runtime_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/runtime"
)

// chatsMemory is a memory that knows the chats a person wrote in, the latest first (domain.PersonChats).
type chatsMemory struct {
	*memMemory
	chats []domain.PersonChat
}

func (m *chatsMemory) ChatsOf(context.Context, string, string) []domain.PersonChat { return m.chats }

// slackLike says a chat whose id starts with C is read by others (a channel), as Slack does.
type slackLike struct{ fakeExecutor }

func (*slackLike) Shared(chat string) bool { return len(chat) > 0 && chat[0] == 'C' }

// What a minion sends unasked reaches the person in the chat they last wrote in, on any chat app it still has; never a
// channel others read unless that is where it was set up; else where it was set up.
func TestReachAt_TheChatTheyLastWroteIn(t *testing.T) {
	tg := &domain.Integration{ID: "int_tg", Service: "telegram"}
	web := &domain.Integration{ID: "int_web", Service: "web"}
	executors := domain.NewExecutorRegistry()
	executors.Register("slack", &slackLike{})
	executors.Register("telegram", &fakeExecutor{})
	executors.Register("web", &fakeExecutor{})
	mem := &chatsMemory{memMemory: newMemMemory()}
	deps := slackDeps(func(d *runtime.AgentDeps) {
		d.Executors = executors
		d.Memory = mem
		d.IntegrationsByID[tg.ID], d.IntegrationsByID[web.ID] = tg, web
		d.IdentitiesByID["id_tg"] = &domain.Identity{ID: "id_tg", IntegrationID: tg.ID}
		d.IdentitiesByID["id_web"] = &domain.Identity{ID: "id_web", IntegrationID: web.ID}
	})
	agent := &domain.Agent{ID: "a1", IdentityIDs: []string{"id_slack", "id_web"}} // Telegram was taken off it
	host := runtime.NewHost()
	host.Register(&runtime.HostedAgent{Agent: agent, Deps: deps})
	ctx := context.Background()

	cases := []struct {
		name     string
		chats    []domain.PersonChat
		setID    string
		setChat  string
		wantID   string
		wantChat string
	}{
		{"the latest chat, not where it was set up", []domain.PersonChat{{"id_web", "v1"}, {"id_slack", "D1"}}, "id_slack", "D1", "id_web", "v1"},
		{"a chat app it no longer has is passed over", []domain.PersonChat{{"id_tg", "77"}, {"id_slack", "D1"}}, "id_web", "v1", "id_slack", "D1"},
		{"a channel others read is passed over", []domain.PersonChat{{"id_slack", "C9"}, {"id_web", "v1"}}, "id_web", "v1", "id_web", "v1"},
		{"unless it was set up there", []domain.PersonChat{{"id_slack", "C9"}, {"id_web", "v1"}}, "id_slack", "C9", "id_slack", "C9"},
		{"nowhere else: where it was set up", nil, "id_slack", "D1", "id_slack", "D1"},
		{"only a channel it wasn't set up in: where it was set up", []domain.PersonChat{{"id_slack", "C9"}}, "id_slack", "D1", "id_slack", "D1"},
	}
	for _, c := range cases {
		mem.chats = c.chats
		id, chat := host.ReachAt(ctx, "a1", "p1", c.setID, c.setChat)
		if id != c.wantID || chat != c.wantChat {
			t.Errorf("%s: got %s/%s, want %s/%s", c.name, id, chat, c.wantID, c.wantChat)
		}
	}
	if id, chat := host.ReachAt(ctx, "a1", "", "id_slack", "D1"); id != "id_slack" || chat != "D1" {
		t.Errorf("nobody: where it was set up, got %s/%s", id, chat)
	}
}
