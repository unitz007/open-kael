package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// world is an empty database with a few agents, bots and tools to hang things on.
func world(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.Empty(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO accounts (id, email, password_hash) VALUES ('creator', 'creator@example.com', 'h')`)
	exec(t, pool, `INSERT INTO integrations (id, name, service) VALUES ('int1', 'Telegram', 'telegram'), ('int2', 'Calendar', 'calendar')`)
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('id1', 'int1'), ('id2', 'int2')`)
	exec(t, pool, `INSERT INTO agents (id, name, created_by) VALUES ('a1', 'One', 'creator'), ('a2', 'Two', 'creator')`)
	exec(t, pool, `INSERT INTO agent_identities (agent_id, identity_id) VALUES ('a1', 'id1')`)
	exec(t, pool, `INSERT INTO tools (id, name, integration_id) VALUES ('t1', 'One', 'int1'), ('t2', 'Two', 'int1')`)
	return postgres.New(pool), pool
}

func person(t *testing.T, s *postgres.Store, id string) {
	t.Helper()
	if err := s.SavePerson(context.Background(), &domain.Person{ID: id}); err != nil {
		t.Fatal(err)
	}
}

func TestAccounts_SaveGetByEmailAndTokenAndSessions(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc1", Email: "a@example.com", PasswordHash: "h", VerificationToken: "tok", PersonID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc3"}); err != nil {
		t.Errorf("logins with no email can coexist: %v", err)
	}
	a, err := s.GetAccountByEmail(ctx, "a@example.com")
	if err != nil || a.ID != "acc1" || a.PersonID != "p1" || a.PasswordHash != "h" || a.CreatedAt.IsZero() {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := s.GetAccountByVerificationToken(ctx, "tok"); err != nil || a.ID != "acc1" {
		t.Errorf("%+v %v", a, err)
	}
	if _, err := s.GetAccountByVerificationToken(ctx, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an empty token never finds anyone: %v", err)
	}
	if _, err := s.GetAccount(ctx, "none"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("not found")
	}
	if got, _ := s.GetAccount(ctx, "acc2"); got.Email != "" || got.PersonID != "" {
		t.Errorf("no email, no person: %+v", got)
	}
	if list, _ := s.ListAccountsByPerson(ctx, "p1"); len(list) != 1 || list[0].ID != "acc1" {
		t.Errorf("%+v", list)
	}

	exp := time.Now().Add(time.Hour)
	if err := s.SaveSession(ctx, &domain.Session{Token: "s1", AccountID: "acc1", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	if sess, err := s.GetSession(ctx, "s1"); err != nil || sess.AccountID != "acc1" {
		t.Errorf("%+v %v", sess, err)
	}
	if err := s.DeleteAccount(ctx, "acc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("deleting a login removes its sessions")
	}
	if count(t, pool, `SELECT count(*) FROM persons WHERE id = 'p1'`) != 1 {
		t.Error("and leaves the person alone")
	}
}

func TestPerson_SaveGetDeleteAndFollowMerge(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	if err := s.SavePerson(ctx, &domain.Person{ID: "p1", FirstName: "Ada", LastName: "Lovelace", Timezone: "Europe/London", Language: "en", Country: "GB", City: "London"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPerson(ctx, "p1")
	if err != nil || p.FullName() != "Ada Lovelace" || p.City != "London" || p.CreatedAt.IsZero() {
		t.Fatalf("%+v %v", p, err)
	}
	person(t, s, "p2")
	if err := s.MergePersons(ctx, "p2", "p1"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetPerson(ctx, "p2"); err != nil || got.ID != "p1" {
		t.Errorf("an old id leads to the person who remains: %+v %v", got, err)
	}
	if _, err := s.GetPerson(ctx, "none"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("not found")
	}

	// Deleting removes what hangs on them, including history that has no foreign key.
	exec(t, pool, `INSERT INTO conversation_messages (agent_id, user_id, role, content) VALUES ('a1', 'p1', 'user', 'hi')`)
	if err := s.DeletePerson(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM conversation_messages WHERE user_id = 'p1'`) != 0 || count(t, pool, `SELECT count(*) FROM persons WHERE id IN ('p1','p2')`) != 0 {
		t.Error("the person, their history and the marker for who was merged into them are gone")
	}
}

func TestEnsurePersonForAccount(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc1", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	p1, err := s.EnsurePersonForAccount(ctx, "acc1")
	if err != nil || p1.ID == "" {
		t.Fatalf("%+v %v", p1, err)
	}
	p2, err := s.EnsurePersonForAccount(ctx, "acc1")
	if err != nil || p2.ID != p1.ID {
		t.Errorf("the same person every time: %v %v", p1.ID, p2.ID)
	}
	if a, _ := s.GetAccount(ctx, "acc1"); a.PersonID != p1.ID {
		t.Errorf("and the login is linked: %+v", a)
	}
	if _, err := s.EnsurePersonForAccount(ctx, "none"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestLinkEmail(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "pa")
	person(t, s, "pb")
	person(t, s, "pc")

	// A new email: a login is made, verified, for this person.
	got, err := s.LinkEmail(ctx, "pa", "a@example.com")
	if err != nil || got != "pa" {
		t.Fatalf("%q %v", got, err)
	}
	a, err := s.GetAccountByEmail(ctx, "a@example.com")
	if err != nil || !a.EmailVerified || a.PersonID != "pa" || a.PasswordHash != "" {
		t.Fatalf("%+v %v", a, err)
	}
	// Again, the same: nothing new.
	if got, err := s.LinkEmail(ctx, "pa", "a@example.com"); err != nil || got != "pa" || count(t, pool, `SELECT count(*) FROM accounts WHERE email = 'a@example.com'`) != 1 {
		t.Errorf("%q %v", got, err)
	}
	// A login that exists but is for nobody yet is taken up.
	exec(t, pool, `INSERT INTO accounts (id, email, password_hash, email_verified) VALUES ('web', 'w@example.com', 'h', TRUE)`)
	if got, err := s.LinkEmail(ctx, "pb", "w@example.com"); err != nil || got != "pb" {
		t.Fatalf("%q %v", got, err)
	}
	if w, _ := s.GetAccount(ctx, "web"); w.PersonID != "pb" || !w.EmailVerified || w.PasswordHash != "h" {
		t.Errorf("linked, verified, password untouched: %+v", w)
	}

	// An email that is another person's login: they are one human, and the two are merged.
	exec(t, pool, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref, sender_id) VALUES ('chc', 'id1', 'pc', '333', '333'), ('chb', 'id1', 'pb', '222', '222')`)
	exec(t, pool, `INSERT INTO conversation_messages (agent_id, user_id, role, content) VALUES ('a1', 'pc', 'user', 'earlier')`)
	got, err = s.LinkEmail(ctx, "pc", "w@example.com")
	if err != nil || got != "pb" {
		t.Fatalf("the person who already had the login remains: %q %v", got, err)
	}
	if count(t, pool, `SELECT count(*) FROM messenger_channels WHERE user_id = 'pb'`) != 2 || count(t, pool, `SELECT count(*) FROM conversation_messages WHERE user_id = 'pb'`) != 1 {
		t.Error("their channels and history moved over")
	}
	if p, _ := s.GetPerson(ctx, "pc"); p.ID != "pb" {
		t.Errorf("the old id leads on: %+v", p)
	}
	if ok, _ := s.PersonHasVerifiedEmail(ctx, "pb"); !ok {
		t.Error("verified")
	}
	if ok, _ := s.PersonHasVerifiedEmail(ctx, "nobody"); ok {
		t.Error("unknown person has none")
	}
	if _, err := s.LinkEmail(ctx, "pa", "  "); err == nil {
		t.Error("no email is an error")
	}
}

func TestUnlinkEmail(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	exec(t, pool, `INSERT INTO accounts (id, email, password_hash, email_verified, person_id) VALUES
		('link_only', 'l@example.com', '', TRUE, 'p1'),
		('with_password', 'pw@example.com', 'h', TRUE, 'p1'),
		('owner', 'o@example.com', '', TRUE, 'p1')`)
	exec(t, pool, `INSERT INTO agents (id, name, created_by) VALUES ('mine', 'Mine', 'owner')`)

	if err := s.UnlinkEmail(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccount(ctx, "link_only"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("a login that was only a link goes away")
	}
	for _, id := range []string{"with_password", "owner"} {
		a, err := s.GetAccount(ctx, id)
		if err != nil || a.PersonID != "" {
			t.Errorf("%s stays as a login, no longer for this person: %+v %v", id, a, err)
		}
	}
	if ok, _ := s.PersonHasVerifiedEmail(ctx, "p1"); ok {
		t.Error("no verified email now")
	}
	if count(t, pool, `SELECT count(*) FROM persons WHERE id = 'p1'`) != 1 {
		t.Error("the person stays")
	}
}

func TestMergePersons_MovesEverythingAndTheSurvivorWins(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	if err := s.RegisterPersonTable("zz_tasks", "user_id"); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `CREATE TABLE zz_tasks (id SERIAL PRIMARY KEY, user_id TEXT NOT NULL, task TEXT NOT NULL, UNIQUE (user_id, task))`)

	// F is merged into T. G was merged into F earlier.
	_ = s.SavePerson(ctx, &domain.Person{ID: "T", FirstName: "Tee"})
	_ = s.SavePerson(ctx, &domain.Person{ID: "F", FirstName: "Eff", LastName: "Fee", Timezone: "Africa/Lagos"})
	person(t, s, "G")
	if err := s.MergePersons(ctx, "G", "F"); err != nil {
		t.Fatal(err)
	}

	exec(t, pool, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('c_t', 'id1', 'T', '1'), ('c_f', 'id1', 'F', '2')`)
	exec(t, pool, `INSERT INTO conversation_messages (agent_id, user_id, role) VALUES ('a1', 'F', 'user'), ('a1', 'F', 'assistant'), ('a1', 'T', 'user')`)
	exec(t, pool, `INSERT INTO app_authorizations (id, identity_id, user_id, credential_ref) VALUES
		('au_t', 'id2', 'T', 'cred_t'), ('au_f1', 'id2', 'F', 'cred_f'), ('au_f2', 'id1', 'F', 'cred_f2')`)
	exec(t, pool, `INSERT INTO user_agent_configs (user_id, agent_id, instructions) VALUES ('T', 'a1', 'T says'), ('F', 'a1', 'F says'), ('F', 'a2', 'F only')`)
	exec(t, pool, `INSERT INTO user_tool_approvals (user_id, tool_id) VALUES ('T', 't1'), ('F', 't1'), ('F', 't2')`)
	exec(t, pool, `INSERT INTO pending_turns (user_id, identity_id, agent_id, conv, message) VALUES ('T', 'id1', 'a1', '{}', 'T ask'), ('F', 'id1', 'a1', '{}', 'F ask')`)
	exec(t, pool, `INSERT INTO profile_details (user_id, scope, key, value) VALUES
		('T', 'personal', 'residence', 'Berlin'), ('F', 'personal', 'residence', 'Lagos'), ('F', 'personal', 'hobbies', 'chess')`)
	exec(t, pool, `INSERT INTO profile_declined (user_id, scope, key, value_norm) VALUES ('F', 'personal', 'residence', 'abuja')`)
	exec(t, pool, `INSERT INTO user_profiles (user_id, notes) VALUES ('T', 'T note'), ('F', 'F note')`)
	exec(t, pool, `INSERT INTO user_integration_notes (user_id, integration_id, notes) VALUES ('F', 'int1', 'F app note'), ('F', 'int2', 'F sorted app note')`)
	exec(t, pool, `INSERT INTO profile_migrations (user_id, scope) VALUES ('T', 'personal'), ('F', 'int2')`)
	exec(t, pool, `INSERT INTO accounts (id, email, person_id) VALUES ('acc_f', 'f@example.com', 'F')`)
	exec(t, pool, `INSERT INTO zz_tasks (user_id, task) VALUES ('T', 'remind'), ('F', 'remind'), ('F', 'report')`)

	if err := s.MergePersons(ctx, "F", "T"); err != nil {
		t.Fatal(err)
	}

	str := func(sql string) string {
		var v string
		if err := pool.QueryRow(ctx, sql).Scan(&v); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
		return v
	}
	for what, c := range map[string]struct {
		sql  string
		want int
	}{
		"both channels":                {`SELECT count(*) FROM messenger_channels WHERE user_id = 'T'`, 2},
		"all history":                  {`SELECT count(*) FROM conversation_messages WHERE user_id = 'T'`, 3},
		"a connection each":            {`SELECT count(*) FROM app_authorizations WHERE user_id = 'T'`, 2},
		"agent settings, T's kept":     {`SELECT count(*) FROM user_agent_configs WHERE user_id = 'T'`, 2},
		"approvals joined":             {`SELECT count(*) FROM user_tool_approvals WHERE user_id = 'T'`, 2},
		"one pending ask":              {`SELECT count(*) FROM pending_turns WHERE user_id = 'T'`, 1},
		"details joined":               {`SELECT count(*) FROM profile_details WHERE user_id = 'T'`, 2},
		"declined moved":               {`SELECT count(*) FROM profile_declined WHERE user_id = 'T'`, 1},
		"tasks joined":                 {`SELECT count(*) FROM zz_tasks WHERE user_id = 'T'`, 2},
		"nothing left on F":            {`SELECT (SELECT count(*) FROM messenger_channels WHERE user_id = 'F') + (SELECT count(*) FROM conversation_messages WHERE user_id = 'F') + (SELECT count(*) FROM app_authorizations WHERE user_id = 'F') + (SELECT count(*) FROM user_agent_configs WHERE user_id = 'F') + (SELECT count(*) FROM profile_details WHERE user_id = 'F') + (SELECT count(*) FROM zz_tasks WHERE user_id = 'F') + (SELECT count(*) FROM user_profiles WHERE user_id = 'F') + (SELECT count(*) FROM user_integration_notes WHERE user_id = 'F')`, 0},
		"F's login now for T":          {`SELECT count(*) FROM accounts WHERE id = 'acc_f' AND person_id = 'T'`, 1},
		"G now points at T":            {`SELECT count(*) FROM persons WHERE id = 'G' AND merged_into = 'T'`, 1},
		"F marks who it was merged to": {`SELECT count(*) FROM persons WHERE id = 'F' AND merged_into = 'T'`, 1},
	} {
		if got := count(t, pool, c.sql); got != c.want {
			t.Errorf("%s: %d, want %d", what, got, c.want)
		}
	}
	if v := str(`SELECT value FROM profile_details WHERE user_id = 'T' AND key = 'residence'`); v != "Berlin" {
		t.Errorf("the survivor's value stands: %q", v)
	}
	if v := str(`SELECT instructions FROM user_agent_configs WHERE user_id = 'T' AND agent_id = 'a1'`); v != "T says" {
		t.Errorf("the survivor's value stands: %q", v)
	}
	if v := str(`SELECT message FROM pending_turns WHERE user_id = 'T'`); v != "T ask" {
		t.Errorf("%q", v)
	}
	if v := str(`SELECT notes FROM user_profiles WHERE user_id = 'T'`); v != "T note\nF note" {
		t.Errorf("older notes are joined, not dropped: %q", v)
	}
	if count(t, pool, `SELECT count(*) FROM profile_migrations WHERE user_id = 'T' AND scope = 'personal'`) != 0 {
		t.Error("the survivor's note holds text that was never sorted, so it is sorted again")
	}
	if v := str(`SELECT notes FROM user_integration_notes WHERE user_id = 'T' AND integration_id = 'int1'`); v != "F app note" {
		t.Errorf("%q", v)
	}
	if count(t, pool, `SELECT count(*) FROM profile_migrations WHERE user_id = 'T' AND scope = 'int2'`) != 1 {
		t.Error("a note that had been sorted keeps its mark")
	}
	var first, last, tz string
	if err := pool.QueryRow(ctx, `SELECT first_name, last_name, timezone FROM persons WHERE id = 'T'`).Scan(&first, &last, &tz); err != nil || first != "Tee" || last != "Fee" || tz != "Africa/Lagos" {
		t.Errorf("their own value stays, gaps are filled: %q %q %q %v", first, last, tz, err)
	}
	if err := pool.QueryRow(ctx, `SELECT first_name, timezone FROM persons WHERE id = 'F'`).Scan(&first, &tz); err != nil || first != "" || tz != "" {
		t.Errorf("the marker holds nothing: %q %q", first, tz)
	}
}

func TestMergePersons_RefusesWhatItCannotDo(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "a")
	person(t, s, "b")
	exec(t, pool, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('c_a', 'id1', 'a', '1')`)
	if err := s.MergePersons(ctx, "a", "a"); err == nil {
		t.Error("a person cannot be merged into themself")
	}
	if err := s.MergePersons(ctx, "a", "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("%v", err)
	}
	if count(t, pool, `SELECT count(*) FROM messenger_channels WHERE user_id = 'a'`) != 1 {
		t.Error("a refused merge moves nothing")
	}
	if err := s.MergePersons(ctx, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.MergePersons(ctx, "a", "b"); err == nil || !strings.Contains(err.Error(), "already merged") {
		t.Errorf("a person who was merged cannot be merged again: %v", err)
	}
	// Merging into someone who was merged away goes to whoever they became.
	person(t, s, "c")
	if err := s.MergePersons(ctx, "c", "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetPerson(ctx, "c"); got.ID != "b" {
		t.Errorf("c ends up in b: %+v", got)
	}
}

func TestMergePersons_ATableThatDoesNotExistIsSkipped(t *testing.T) {
	s, _ := world(t)
	ctx := context.Background()
	if err := s.RegisterPersonTable("not_made_yet", "user_id"); err != nil {
		t.Fatal(err)
	}
	person(t, s, "a")
	person(t, s, "b")
	if err := s.MergePersons(ctx, "a", "b"); err != nil {
		t.Errorf("a registered table that was never created must not stop a merge: %v", err)
	}
	if err := s.DeletePerson(ctx, "b"); err != nil {
		t.Errorf("nor a delete: %v", err)
	}
}

func TestRegisterPersonTable_RejectsOddNames(t *testing.T) {
	s, _ := world(t)
	for _, bad := range [][2]string{{"users; drop table persons", "user_id"}, {"t", "id) --"}, {"", "x"}} {
		if err := s.RegisterPersonTable(bad[0], bad[1]); err == nil {
			t.Errorf("%v should be refused", bad)
		}
	}
}

// Someone who registers with another person's email must not gain that person by waiting for them to
// prove they own it. Registering proves nothing: only the verification link does.
func TestLinkEmail_AnEmailRegisteredByAStrangerDoesNotHandThemTheOwner(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "attacker_person")
	person(t, s, "victim")
	exec(t, pool, `INSERT INTO accounts (id, email, password_hash, email_verified, verification_token, person_id)
		VALUES ('evil', 'victim@example.com', 'attackers-password', FALSE, 'tok', 'attacker_person')`)
	exec(t, pool, `INSERT INTO user_sessions (token, account_id, expires_at) VALUES ('evil_session', 'evil', now() + interval '1 day')`)
	exec(t, pool, `INSERT INTO messenger_channels (id, identity_id, user_id, channel_ref) VALUES ('cv', 'id1', 'victim', '1')`)
	exec(t, pool, `INSERT INTO conversation_messages (agent_id, user_id, role, content) VALUES ('a1', 'victim', 'user', 'private')`)
	exec(t, pool, `INSERT INTO app_authorizations (id, identity_id, user_id, credential_ref) VALUES ('au', 'id2', 'victim', 'victims-token')`)

	got, err := s.LinkEmail(ctx, "victim", "victim@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "victim" {
		t.Errorf("the person who proved the email stays themself, not folded into the stranger's: %q", got)
	}
	acc, err := s.GetAccount(ctx, "evil")
	if err != nil || acc.PersonID != "victim" || !acc.EmailVerified {
		t.Fatalf("the login now belongs to the person who proved the email: %+v %v", acc, err)
	}
	if acc.PasswordHash != "" || acc.VerificationToken != "" {
		t.Errorf("what the stranger set is wiped: password %q, token %q", acc.PasswordHash, acc.VerificationToken)
	}
	if _, err := s.GetSession(ctx, "evil_session"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("and their sessions are ended")
	}
	for what, sql := range map[string]string{
		"the victim's chat stays theirs":       `SELECT count(*) FROM messenger_channels WHERE user_id = 'victim'`,
		"the victim's history stays theirs":    `SELECT count(*) FROM conversation_messages WHERE user_id = 'victim'`,
		"the victim's connection stays theirs": `SELECT count(*) FROM app_authorizations WHERE user_id = 'victim'`,
		"the stranger's person is untouched":   `SELECT count(*) FROM persons WHERE id = 'attacker_person' AND merged_into IS NULL`,
	} {
		if count(t, pool, sql) != 1 {
			t.Errorf("%s", what)
		}
	}
	if p, _ := s.GetPerson(ctx, "victim"); p.ID != "victim" {
		t.Errorf("%+v", p)
	}
}

func TestSaveAppAuthorization_AConnectionStartedBeforeAMergeLandsOnTheSurvivor(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "gone")
	person(t, s, "stays")
	if err := s.MergePersons(ctx, "gone", "stays"); err != nil {
		t.Fatal(err)
	}
	// The sign-in began while "gone" was still a person, and comes back after the merge.
	if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: "au1", IdentityID: "id2", UserID: "gone", CredentialRef: "ref"}); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM app_authorizations WHERE id = 'au1' AND user_id = 'stays'`) != 1 {
		t.Error("the connection is the surviving person's, where they will see it")
	}
	// A person who was never merged is untouched, and an unknown one is still refused.
	if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: "au2", IdentityID: "id1", UserID: "stays"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: "au3", IdentityID: "id1", UserID: "nobody"}); err == nil {
		t.Error("a connection for no one is refused")
	}
}

func TestEmails_AreOneLoginWhateverTheCase(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	person(t, s, "p2")

	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc1", Email: "  Ada@Example.COM ", PersonID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if count(t, pool, `SELECT count(*) FROM accounts WHERE email = 'ada@example.com'`) != 1 {
		t.Error("kept lower case and trimmed")
	}
	for _, typed := range []string{"ada@example.com", "ADA@EXAMPLE.COM", " Ada@Example.com\n"} {
		if a, err := s.GetAccountByEmail(ctx, typed); err != nil || a.ID != "acc1" {
			t.Errorf("found by %q: %+v %v", typed, a, err)
		}
	}
	// The same address in another case is the same login, so it cannot be a second one.
	if err := s.SaveAccount(ctx, &domain.Account{ID: "acc2", Email: "ADA@example.com"}); err == nil {
		t.Error("a second login for the same address, in another case, is refused")
	}
	// Linking finds the existing login whatever the case, and never makes a second.
	exec(t, pool, `UPDATE accounts SET email_verified = TRUE WHERE id = 'acc1'`)
	got, err := s.LinkEmail(ctx, "p2", "ADA@Example.com")
	if err != nil || got != "p1" {
		t.Fatalf("the verified login's person remains: %q %v", got, err)
	}
	if count(t, pool, `SELECT count(*) FROM accounts WHERE lower(email) = 'ada@example.com'`) != 1 {
		t.Error("still one login for that address")
	}
	// A link request keeps the address the same way, so the page and the link agree.
	if err := s.CreateEmailVerification(ctx, &domain.EmailVerification{Token: "t", PersonID: "p1", Email: " NEW@Example.com ", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.GetEmailVerificationByToken(ctx, "t"); ev.Email != "new@example.com" {
		t.Errorf("%q", ev.Email)
	}
	// An empty address is no address, and several logins without one can coexist.
	for _, id := range []string{"n1", "n2"} {
		if err := s.SaveAccount(ctx, &domain.Account{ID: id, Email: "   "}); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestMigrate_OlderEmailsAreBroughtToLowerCaseAndCollisionsLeftAlone(t *testing.T) {
	pool := pgtest.Empty(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// As older versions stored them.
	exec(t, pool, `INSERT INTO accounts (id, email) VALUES
		('mixed', 'Mixed@Example.com'), ('spaced', ' spaced@example.com '), ('fine', 'fine@example.com'),
		('twin1', 'Twin@Example.com'), ('twin2', 'twin@example.com'), ('twin3', 'TWIN@example.com')`)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("a collision must not stop the server starting: %v", err)
	}
	str := func(id string) string {
		var e string
		if err := pool.QueryRow(ctx, `SELECT email FROM accounts WHERE id = $1`, id).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	if str("mixed") != "mixed@example.com" || str("spaced") != "spaced@example.com" || str("fine") != "fine@example.com" {
		t.Errorf("older rows are normalised: %q %q %q", str("mixed"), str("spaced"), str("fine"))
	}
	if str("twin1") != "Twin@Example.com" || str("twin2") != "twin@example.com" || str("twin3") != "TWIN@example.com" {
		t.Errorf("the same address three times is left for a person to settle, not guessed: %q %q %q", str("twin1"), str("twin2"), str("twin3"))
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Errorf("and it is repeatable: %v", err)
	}
}

// When both have a connection to the same app, the newer one stands, whichever person remains: someone who connects an
// app on the web and then carries on in Telegram, where an old connection to it lingers, keeps the one they just made.
func TestMergePersons_TheNewerConnectionWins(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "web")
	person(t, s, "tg")
	person(t, s, "x1")
	person(t, s, "x2")
	exec(t, pool, `INSERT INTO app_authorizations (id, identity_id, user_id, credential_ref, updated_at) VALUES
		('old_fpl', 'id1', 'tg', 'stale', NOW() - INTERVAL '90 days'),
		('new_fpl', 'id1', 'web', 'fresh', NOW()),
		('tg_only', 'id2', 'tg', 'mail', NULL),
		('x1_new', 'id1', 'x1', 'newer', NOW()),
		('x2_unknown', 'id1', 'x2', 'older-unknown', NULL)`)
	if err := s.MergePersons(ctx, "web", "tg"); err != nil {
		t.Fatal(err)
	}
	var cred string
	if err := pool.QueryRow(ctx, `SELECT credential_ref FROM app_authorizations WHERE user_id = 'tg' AND identity_id = 'id1'`).Scan(&cred); err != nil || cred != "fresh" {
		t.Errorf("the one just made stands: %q %v", cred, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM app_authorizations WHERE user_id = 'tg'`).Scan(&n)
	if n != 2 {
		t.Errorf("the rest stays: %d", n)
	}
	// One saved with a time beats one saved before times were kept.
	if err := s.MergePersons(ctx, "x1", "x2"); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT credential_ref FROM app_authorizations WHERE user_id = 'x2' AND identity_id = 'id1'`).Scan(&cred)
	if cred != "newer" {
		t.Errorf("a connection with a time beats one without: %q", cred)
	}
	// And a saved connection is stamped.
	if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: "stamped", IdentityID: "id2", UserID: "x2", CredentialRef: "c"}); err != nil {
		t.Fatal(err)
	}
	var stamped bool
	_ = pool.QueryRow(ctx, `SELECT updated_at IS NOT NULL FROM app_authorizations WHERE id = 'stamped'`).Scan(&stamped)
	if !stamped {
		t.Error("saving stamps the time")
	}
}
