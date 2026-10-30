package domain

import (
	"fmt"
	"testing"
)

func user(s string) Message  { return Message{Role: RoleUser, Content: s} }
func reply(s string) Message { return Message{Role: RoleAssistant, Content: s} }
func lookup(id string) []Message {
	return []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "get_my_team"}}},
		{Role: RoleTool, ToolCallID: id, Name: "get_my_team", Content: `{"players":[]}`},
	}
}

func show(ms []Message) string {
	s := ""
	for _, m := range ms {
		switch {
		case m.Role == RoleTool:
			s += "[result " + m.ToolCallID + "]"
		case len(m.ToolCalls) > 0:
			s += "[call " + m.ToolCalls[0].ID + "]"
		default:
			s += fmt.Sprintf("[%s %s]", m.Role, m.Content)
		}
	}
	return s
}

func TestRecentWindow_TheLatestExchangeWholeAndBeforeItTheWords(t *testing.T) {
	var msgs []Message
	msgs = append(msgs, user("a"))
	msgs = append(msgs, lookup("c1")...)
	msgs = append(msgs, Message{Role: RoleSystem, Content: "The person wants advice"}, reply("A"))
	msgs = append(msgs, user("b"), reply("B"))
	msgs = append(msgs, user("c"))
	msgs = append(msgs, lookup("c3")...)
	msgs = append(msgs, Message{Role: RoleSystem, Content: "The person wants advice"}, reply("C"))

	got := show(RecentWindow(msgs, 1, 10))
	want := "[user a][assistant A][user b][assistant B][user c][call c3][result c3][assistant C]"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Fewer words wanted: the oldest go first, the latest exchange always stays whole.
	if got, want := show(RecentWindow(msgs, 1, 3)), "[assistant B][user c][call c3][result c3][assistant C]"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRecentWindow_NeverStartsOnAResultWithoutItsCall(t *testing.T) {
	// Read from the middle of an exchange: its result without its call.
	msgs := []Message{{Role: RoleTool, ToolCallID: "c0", Content: "{}"}, reply("A"), user("b")}
	msgs = append(msgs, lookup("c1")...)
	msgs = append(msgs, reply("B"))
	if got, want := show(RecentWindow(msgs, 1, 10)), "[assistant A][user b][call c1][result c1][assistant B]"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// No message from the person in what was read: words only.
	only := []Message{{Role: RoleTool, ToolCallID: "c0", Content: "{}"}, reply("A"), {Role: RoleAssistant, Content: "and", ToolCalls: []ToolCall{{ID: "c2"}}}}
	if got, want := show(RecentWindow(only, 1, 10)), "[assistant A][assistant and]"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if len(RecentWindow(nil, 1, 10)) != 0 {
		t.Fatal("nothing from nothing")
	}
}

func TestRecentWindow_SeveralWholeExchanges(t *testing.T) {
	var msgs []Message
	msgs = append(msgs, user("a"))
	msgs = append(msgs, lookup("c1")...)
	msgs = append(msgs, reply("A"), user("b"))
	msgs = append(msgs, lookup("c2")...)
	msgs = append(msgs, reply("B"), user("c"))
	msgs = append(msgs, lookup("c3")...)
	msgs = append(msgs, Message{Role: RoleSystem, Content: "note"}, reply("C"))
	if got, want := show(RecentWindow(msgs, 2, 10)), "[user a][assistant A][user b][call c2][result c2][assistant B][user c][call c3][result c3][assistant C]"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got, want := show(RecentWindow(msgs, 5, 10)), "[user a][call c1][result c1][assistant A][user b][call c2][result c2][assistant B][user c][call c3][result c3][assistant C]"; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
