package postgres_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/unitz007/open-kael/domain"
)

func peopleFor(t *testing.T, s domain.PeopleStore, conn, kind, value string) []string {
	t.Helper()
	got, err := s.PeopleFor(context.Background(), conn, kind, value)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An identifier a source uses finds every person linked to it, ignoring case where the source does, and only on the
// connection it was linked on.
func TestExternalIdentities_OneToMany(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	person(t, s, "p2")
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('gh1', 'int1'), ('gh2', 'int1')`)

	for _, id := range []domain.ExternalIdentity{
		{ConnectionID: "gh1", Kind: "github_account", Value: "Octo", PersonID: "p1"},
		{ConnectionID: "gh1", Kind: "github_account", Value: "octo", PersonID: "p2"},
		{ConnectionID: "gh1", Kind: "github_account", Value: "octo", PersonID: "p2"}, // again: no-op
		{ConnectionID: "gh2", Kind: "github_account", Value: "octo", PersonID: "p1"},
		{ConnectionID: "gh1", Kind: "stripe_customer", Value: "cus_A", PersonID: "p1"},
	} {
		if err := s.Link(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := peopleFor(t, s, "gh1", "github_account", "OCTO"); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("both people, either case: %v", got)
	}
	if got := peopleFor(t, s, "gh2", "github_account", "octo"); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("another connection has its own people: %v", got)
	}
	if got := peopleFor(t, s, "gh1", "github_account", "someone-else"); len(got) != 0 {
		t.Fatalf("an unknown identifier is nobody: %v", got)
	}
	if got := peopleFor(t, s, "gh1", "stripe_customer", "cus_a"); len(got) != 0 {
		t.Fatalf("a kind that is case sensitive is not folded: %v", got)
	}
	if got := peopleFor(t, s, "gh1", "stripe_customer", "cus_A"); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("an exact match: %v", got)
	}
	if err := s.Link(ctx, domain.ExternalIdentity{ConnectionID: "gh1", Kind: "github_account", Value: "  ", PersonID: "p1"}); err == nil {
		t.Fatal("an empty identifier is refused")
	}
}

// Saving a connected account links the person; changing, moving or deleting it keeps the link true; a service nobody
// registered is not linked.
func TestExternalIdentities_FollowConnectedAccounts(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	person(t, s, "p2")
	exec(t, pool, `INSERT INTO integrations (id, name, service) VALUES ('ghi', 'GitHub', 'github')`)
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('gh1', 'ghi')`)
	s.RegisterIdentityKind("github", "github_account")

	save := func(id, person, login string) {
		t.Helper()
		if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: id, IdentityID: "gh1", UserID: person, ExternalUserID: login}); err != nil {
			t.Fatal(err)
		}
	}
	save("a1", "p1", "Octo")
	save("a2", "p2", "octo")
	if got := peopleFor(t, s, "gh1", "github_account", "octo"); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("both connected: %v", got)
	}
	save("a1", "p1", "renamed") // the same connection now shows another login
	if got := peopleFor(t, s, "gh1", "github_account", "octo"); !reflect.DeepEqual(got, []string{"p2"}) {
		t.Fatalf("the old login no longer finds p1: %v", got)
	}
	if got := peopleFor(t, s, "gh1", "github_account", "renamed"); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("the new one does: %v", got)
	}
	save("a1", "p1", "") // no login any more
	if got := peopleFor(t, s, "gh1", "github_account", "renamed"); len(got) != 0 {
		t.Fatalf("no login, no link: %v", got)
	}
	if err := s.DeleteAppAuthorization(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if got := peopleFor(t, s, "gh1", "github_account", "octo"); len(got) != 0 {
		t.Fatalf("disconnecting removes the link: %v", got)
	}

	// An account on a service that was not registered is left alone.
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('tg', 'int1')`)
	if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: "a3", IdentityID: "tg", UserID: "p1", ExternalUserID: "42"}); err != nil {
		t.Fatal(err)
	}
	if got := peopleFor(t, s, "tg", "github_account", "42"); len(got) != 0 {
		t.Fatalf("not linked: %v", got)
	}
}

// Accounts connected before this existed are linked once, and running it again changes nothing.
func TestExternalIdentities_Backfill(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "p1")
	person(t, s, "p2")
	exec(t, pool, `INSERT INTO integrations (id, name, service) VALUES ('ghi', 'GitHub', 'github')`)
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('gh1', 'ghi'), ('tg', 'int1')`)
	exec(t, pool, `INSERT INTO app_authorizations (id, identity_id, user_id, external_user_id) VALUES
		('a1', 'gh1', 'p1', ' Octo '), ('a2', 'gh1', 'p2', ''), ('a3', 'tg', 'p1', '42')`)
	if _, err := s.BackfillExternalIdentities(ctx, "github"); err == nil {
		t.Fatal("a service with no registered kind cannot be backfilled")
	}
	s.RegisterIdentityKind("github", "github_account")
	n, err := s.BackfillExternalIdentities(ctx, "github")
	if err != nil || n != 1 {
		t.Fatalf("one account has a login and is GitHub: n=%d err=%v", n, err)
	}
	if got := peopleFor(t, s, "gh1", "github_account", "octo"); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("linked, trimmed and folded: %v", got)
	}
	if n, err := s.BackfillExternalIdentities(ctx, "github"); err != nil || n != 0 {
		t.Fatalf("again: n=%d err=%v", n, err)
	}
	if got := peopleFor(t, s, "tg", "github_account", "42"); len(got) != 0 {
		t.Fatalf("other services are not touched: %v", got)
	}
}

// Merging two people carries their identifiers to the one who remains; both being linked to the same one leaves one row.
func TestExternalIdentities_MergeMovesThem(t *testing.T) {
	s, pool := world(t)
	ctx := context.Background()
	person(t, s, "from")
	person(t, s, "to")
	exec(t, pool, `INSERT INTO integrations (id, name, service) VALUES ('ghi', 'GitHub', 'github')`)
	exec(t, pool, `INSERT INTO identities (id, integration_id) VALUES ('gh1', 'ghi')`)
	s.RegisterIdentityKind("github", "github_account")
	for id, who := range map[string]string{"a1": "from", "a2": "to"} {
		if err := s.SaveAppAuthorization(ctx, &domain.AppAuthorization{ID: id, IdentityID: "gh1", UserID: who, ExternalUserID: "octo"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Link(ctx, domain.ExternalIdentity{ConnectionID: "gh1", Kind: "email", Value: "A@x.com", PersonID: "from"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MergePersons(ctx, "from", "to"); err != nil {
		t.Fatal(err)
	}
	if got := peopleFor(t, s, "gh1", "github_account", "octo"); !reflect.DeepEqual(got, []string{"to"}) {
		t.Fatalf("one person now: %v", got)
	}
	if got := peopleFor(t, s, "gh1", "email", "a@x.com"); !reflect.DeepEqual(got, []string{"to"}) {
		t.Fatalf("an identifier only the merged person had moves: %v", got)
	}
}
