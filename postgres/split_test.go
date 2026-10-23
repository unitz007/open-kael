package postgres_test

import (
	"context"
	_ "embed"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

//go:embed testdata/schema_before_split.sql
var schemaBeforeSplit string

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%v\n%s", err, sql)
	}
	return n
}

// An older database, with the single users table, is split into persons and accounts without losing
// or rewriting anything that pointed at a user.
func TestMigrate_SplitsOldUsersIntoPersonsAndAccounts(t *testing.T) {
	pool := pgtest.Empty(t)
	ctx := context.Background()
	exec(t, pool, schemaBeforeSplit)

	// What an old database holds: a creator who logs in and owns an agent, someone who writes from a
	// messenger and has learned things, a messenger user who linked an email (no password), a
	// session-only row, and an empty row.
	exec(t, pool, `INSERT INTO users (id, email, password_hash, first_name, last_name, email_verified, timezone) VALUES
		('u_creator', 'c@example.com', 'hash', 'Cre', 'Ator', TRUE, 'Africa/Lagos'),
		('u_chat', NULL, '', '', '', FALSE, 'Europe/Berlin'),
		('u_link', 'l@example.com', '', 'Lin', '', TRUE, ''),
		('u_sess', NULL, '', '', '', FALSE, ''),
		('u_empty', NULL, '', '', '', FALSE, '')`)
	exec(t, pool, `UPDATE users SET city = 'Wolfsburg', country = 'DE', language = 'de' WHERE id = 'u_chat'`)
	exec(t, pool, `INSERT INTO agents (id, name, created_by) VALUES ('a1', 'Coach', 'u_creator')`)
	exec(t, pool, `INSERT INTO integrations (id, name, service, created_by) VALUES ('int1', 'Telegram', 'telegram', 'u_creator')`)
	exec(t, pool, `INSERT INTO identities (id, integration_id, created_by) VALUES ('id1', 'int1', 'u_creator')`)
	exec(t, pool, `INSERT INTO agent_identities (agent_id, identity_id) VALUES ('a1', 'id1')`)
	exec(t, pool, `INSERT INTO user_sessions (token, user_id, expires_at) VALUES ('tok_c', 'u_creator', now() + interval '1 day'), ('tok_s', 'u_sess', now() + interval '1 day')`)
	exec(t, pool, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref, sender_id) VALUES
		('ch1', 'id1', 'u_chat', '111', '111'), ('ch2', 'id1', 'u_link', '222', '222')`)
	exec(t, pool, `INSERT INTO user_profiles (user_id, notes) VALUES ('u_chat', 'likes tea')`)
	exec(t, pool, `INSERT INTO user_integration_notes (user_id, integration_id, notes) VALUES ('u_chat', 'int1', 'team: x')`)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if count(t, pool, `SELECT count(*) FROM persons`) != 5 {
		t.Errorf("every old user is a person")
	}
	var tz, city, country, lang, first string
	if err := pool.QueryRow(ctx, `SELECT timezone, city, country, language FROM persons WHERE id = 'u_chat'`).Scan(&tz, &city, &country, &lang); err != nil || tz != "Europe/Berlin" || city != "Wolfsburg" || country != "DE" || lang != "de" {
		t.Errorf("what they told us is kept: %v %v %v %v %v", tz, city, country, lang, err)
	}
	if err := pool.QueryRow(ctx, `SELECT first_name FROM persons WHERE id = 'u_creator'`).Scan(&first); err != nil || first != "Cre" {
		t.Errorf("names move to the person: %q %v", first, err)
	}

	// Logins: those that had an email or password, a session, or owned something.
	if n := count(t, pool, `SELECT count(*) FROM accounts`); n != 3 {
		t.Errorf("three of the five acted as logins: %d", n)
	}
	for _, id := range []string{"u_creator", "u_link", "u_sess"} {
		if count(t, pool, `SELECT count(*) FROM accounts WHERE id = $1 AND person_id = $1`, id) != 1 {
			t.Errorf("%s is a login for its own person", id)
		}
	}
	for _, id := range []string{"u_chat", "u_empty"} {
		if count(t, pool, `SELECT count(*) FROM accounts WHERE id = $1`, id) != 0 {
			t.Errorf("%s never logged in and gets no account", id)
		}
	}
	var email, hash string
	var verified bool
	if err := pool.QueryRow(ctx, `SELECT email, password_hash, email_verified FROM accounts WHERE id = 'u_creator'`).Scan(&email, &hash, &verified); err != nil || email != "c@example.com" || hash != "hash" || !verified {
		t.Errorf("login details carried over: %v %v %v %v", email, hash, verified, err)
	}
	if count(t, pool, `SELECT count(*) FROM accounts WHERE id = 'u_sess' AND email IS NULL`) != 1 {
		t.Errorf("no email stays no email, so two such logins can coexist")
	}

	// Things that pointed at a user still point at the same id, now at the right kind of row.
	if count(t, pool, `SELECT count(*) FROM agents WHERE created_by = 'u_creator'`) != 1 || count(t, pool, `SELECT count(*) FROM user_sessions WHERE account_id = 'u_creator'`) != 1 {
		t.Error("creator's agent and session intact")
	}
	for what, sql := range map[string]string{
		"two channels": `SELECT count(*) FROM messenger_channels WHERE user_id IN ('u_chat','u_link')`,
		"a note":       `SELECT count(*) FROM user_profiles WHERE user_id = 'u_chat'`,
		"an app note":  `SELECT count(*) FROM user_integration_notes WHERE user_id = 'u_chat'`,
	} {
		want := 1
		if what == "two channels" {
			want = 2
		}
		if got := count(t, pool, sql); got != want {
			t.Errorf("%s: %d, want %d", what, got, want)
		}
	}

	// Tables that are new in this version belong to persons from the start.
	exec(t, pool, `INSERT INTO profile_details (user_id, scope, key, value) VALUES ('u_chat', 'personal', 'hobbies', 'chess')`)

	// Foreign keys were moved, not lost.
	if _, err := pool.Exec(ctx, `INSERT INTO agents (id, name, created_by) VALUES ('a2', 'X', 'u_chat')`); err == nil {
		t.Error("created_by must point at a login: a person with no login cannot own an agent")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('ch9', 'id1', 'no_such', '9')`); err == nil {
		t.Error("a channel must belong to a person")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_sessions (token, account_id, expires_at) VALUES ('t9', 'u_chat', now())`); err == nil {
		t.Error("a session must belong to a login")
	}
	exec(t, pool, `DELETE FROM persons WHERE id = 'u_chat'`)
	if count(t, pool, `SELECT count(*) FROM user_profiles WHERE user_id = 'u_chat'`)+count(t, pool, `SELECT count(*) FROM messenger_channels WHERE user_id = 'u_chat'`)+count(t, pool, `SELECT count(*) FROM profile_details WHERE user_id = 'u_chat'`) != 0 {
		t.Error("deleting a person still removes what hangs on them")
	}
	exec(t, pool, `DELETE FROM accounts WHERE id = 'u_sess'`)
	if count(t, pool, `SELECT count(*) FROM user_sessions WHERE token = 'tok_s'`) != 0 {
		t.Error("deleting a login removes its sessions")
	}

	// The old table is kept, renamed, as a backup. Nothing is left called users.
	if count(t, pool, `SELECT count(*) FROM users_before_split`) != 5 {
		t.Error("the old table is kept")
	}
	if count(t, pool, `SELECT count(*) FROM pg_class WHERE relname = 'users' AND relkind = 'r'`) != 0 {
		t.Error("no users table remains")
	}

	// And it only ever happens once.
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if count(t, pool, `SELECT count(*) FROM persons`) != 4 || count(t, pool, `SELECT count(*) FROM accounts`) != 2 {
		t.Error("a second run changes nothing")
	}
}

// A new database starts with persons and accounts and never has a users table.
func TestMigrate_NewDatabaseHasNoUsersTable(t *testing.T) {
	pool := pgtest.Empty(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM pg_class WHERE relname IN ('users','users_before_split') AND relkind = 'r'`) != 0 {
		t.Error("a new database has no users table")
	}
	if count(t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'user_sessions' AND column_name = 'account_id'`) != 1 {
		t.Error("sessions belong to accounts")
	}
}
