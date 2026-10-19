package postgres_test

import (
	"context"
	"testing"
	"time"
)

// oldPerson makes a person who has been there for days, with nothing yet.
func oldPerson(t *testing.T, id string, run func(string, ...any)) {
	t.Helper()
	run(`INSERT INTO persons (id, created_at) VALUES ($1, now() - interval '3 days')`, id)
}

func exists(t *testing.T, run func(string, ...any), count func(string, ...any) int, id string) bool {
	t.Helper()
	return count(`SELECT count(*) FROM persons WHERE id = $1`, id) == 1
}

func TestDeleteEmptyPeople_RemovesOnlyThoseWithNothing(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	run := func(sql string, args ...any) { exec(t, pool, sql, args...) }
	cnt := func(sql string, args ...any) int { return count(t, pool, sql, args...) }
	if err := s.RegisterPersonTable("zz_things", "user_id"); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterPersonTable("not_made_yet", "user_id"); err != nil {
		t.Fatal(err)
	}
	run(`CREATE TABLE zz_things (id SERIAL PRIMARY KEY, user_id TEXT NOT NULL)`)

	// Empty: nothing at all, a blank note, a mark that a note was sorted, and a refused value.
	for _, id := range []string{"empty1", "empty2", "blank_note", "only_marks"} {
		oldPerson(t, id, run)
	}
	run(`INSERT INTO user_profiles (user_id, notes) VALUES ('blank_note', '  ')`)
	run(`INSERT INTO user_integration_notes (user_id, integration_id, notes) VALUES ('blank_note', 'int1', '')`)
	run(`INSERT INTO profile_migrations (user_id, scope) VALUES ('only_marks', 'personal')`)
	run(`INSERT INTO profile_declined (user_id, scope, key, value_norm) VALUES ('only_marks', 'personal', 'k', 'v')`)

	// Each of these has one trace, and must stay.
	traces := map[string]string{
		"has_channel":      `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('c1', 'id1', 'has_channel', '1')`,
		"has_message":      `INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'has_message', 'user')`,
		"has_old_channel":  `INSERT INTO user_channels (id, user_id, provider, channel_id) VALUES ('uc', 'has_old_channel', 'telegram', '9')`,
		"has_link_request": `INSERT INTO email_verifications (token, user_id, expires_at) VALUES ('tok', 'has_link_request', now() + interval '1 day')`,
		"has_connection":   `INSERT INTO app_authorizations (id, identity_id, user_id) VALUES ('au', 'id2', 'has_connection')`,
		"has_agent_config": `INSERT INTO user_agent_configs (user_id, agent_id, instructions) VALUES ('has_agent_config', 'a1', 'x')`,
		"has_approval":     `INSERT INTO user_tool_approvals (user_id, tool_id) VALUES ('has_approval', 't1')`,
		"has_app_instr":    `INSERT INTO user_app_instructions (user_id, integration_id, instructions) VALUES ('has_app_instr', 'int1', 'x')`,
		"has_pending":      `INSERT INTO pending_turns (user_id, identity_id, agent_id, conv, message) VALUES ('has_pending', 'id1', 'a1', '{}', 'm')`,
		"has_detail":       `INSERT INTO profile_details (user_id, scope, key, value) VALUES ('has_detail', 'personal', 'k', 'v')`,
		"has_login":        `INSERT INTO accounts (id, email, person_id) VALUES ('acc', 'a@example.com', 'has_login')`,
		"has_note":         `INSERT INTO user_profiles (user_id, notes) VALUES ('has_note', 'likes tea')`,
		"has_app_note":     `INSERT INTO user_integration_notes (user_id, integration_id, notes) VALUES ('has_app_note', 'int1', 'team x')`,
		"has_task":         `INSERT INTO zz_things (user_id) VALUES ('has_task')`,
		"has_name":         `UPDATE persons SET first_name = 'Ada' WHERE id = 'has_name'`,
		"has_zone":         `UPDATE persons SET timezone = 'Africa/Lagos' WHERE id = 'has_zone'`,
		"has_city":         `UPDATE persons SET city = 'Lagos' WHERE id = 'has_city'`,
		"was_merged_into":  `UPDATE persons SET merged_into = 'was_merged_into' WHERE id = 'merged_away'`,
	}
	for id, sql := range traces {
		oldPerson(t, id, run)
		if id == "was_merged_into" {
			oldPerson(t, "merged_away", run)
		}
		run(sql)
	}

	// Someone who only just arrived is left alone, however empty.
	run(`INSERT INTO persons (id) VALUES ('just_arrived')`)

	removed, err := s.DeleteEmptyPeople(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 {
		t.Errorf("the four with nothing are removed: %d", removed)
	}
	for _, id := range []string{"empty1", "empty2", "blank_note", "only_marks"} {
		if exists(t, run, cnt, id) {
			t.Errorf("%s had nothing and should be gone", id)
		}
	}
	for id := range traces {
		if !exists(t, run, cnt, id) {
			t.Errorf("%s has a trace and must stay", id)
		}
	}
	if !exists(t, run, cnt, "merged_away") {
		t.Error("the marker for a merged person stays, so old ids still lead somewhere")
	}
	if !exists(t, run, cnt, "just_arrived") {
		t.Error("someone who arrived a moment ago is never caught in the middle of a first message")
	}

	// With no grace they go too, and a second run finds nothing.
	if n, err := s.DeleteEmptyPeople(ctx, 0); err != nil || n != 1 {
		t.Errorf("with no grace period the newcomer goes too: %d %v", n, err)
	}
	if n, _ := s.DeleteEmptyPeople(ctx, 0); n != 0 {
		t.Errorf("nothing left to remove: %d", n)
	}
	if p, err := s.GetPerson(ctx, "merged_away"); err != nil || p.ID != "was_merged_into" {
		t.Errorf("%+v %v", p, err)
	}
}

func TestDeleteEmptyPeople_WhatIsLeftWhenABotIsDeleted(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	run := func(sql string, args ...any) { exec(t, pool, sql, args...) }
	run(`INSERT INTO identities (id, integration_id) VALUES ('gone_bot', 'int1')`)
	oldPerson(t, "wrote_to_gone_bot", run)
	oldPerson(t, "wrote_to_both", run)
	run(`INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('c1', 'gone_bot', 'wrote_to_gone_bot', '1'), ('c2', 'gone_bot', 'wrote_to_both', '2'), ('c3', 'id1', 'wrote_to_both', '3')`)

	// The bot is deleted: its channels go with it (that already happens), the people do not.
	run(`DELETE FROM identities WHERE id = 'gone_bot'`)
	if count(t, pool, `SELECT count(*) FROM persons WHERE id IN ('wrote_to_gone_bot','wrote_to_both')`) != 2 {
		t.Fatal("the people are still there")
	}
	if n, err := s.DeleteEmptyPeople(ctx, time.Hour); err != nil || n != 1 {
		t.Errorf("the person who only wrote to the deleted bot is removed: %d %v", n, err)
	}
	if count(t, pool, `SELECT count(*) FROM persons WHERE id = 'wrote_to_both'`) != 1 {
		t.Error("the person who also wrote to another bot stays")
	}
}
