package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres"
)

// A restart (Migrate) must never mark a person as welcomed: NULL onboarded_at means they
// still have to be welcomed, or were reset on purpose. The backfill for channels that
// predate onboarding runs only when the column is first created.
func TestMigrate_RestartsNeverMarkAPersonWelcomed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := s.SavePerson(ctx, &domain.Person{ID: "u_mig"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIntegration(ctx, &domain.Integration{ID: "int_mig", Name: "Telegram", Service: "telegram"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity(ctx, &domain.Identity{ID: "id_mig", IntegrationID: "int_mig", Name: "Account", Kind: "oauth"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMessengerChannel(ctx, &domain.MessengerChannel{ID: "mc_mig", IdentityID: "id_mig", UserID: "u_mig", ChannelRef: "42"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeletePerson(ctx, "u_mig") })

	welcomed := func() bool {
		ch, err := s.GetMessengerChannelByIdentityAndRef(ctx, "id_mig", "42")
		if err != nil {
			t.Fatal(err)
		}
		return ch.OnboardedAt != nil
	}

	// A new person, not yet welcomed, survives any number of restarts.
	for i := 0; i < 3; i++ {
		if err := postgres.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
		if welcomed() {
			t.Fatalf("restart %d marked an unwelcomed person as welcomed", i+1)
		}
	}

	// The first time the column appears, channels that already existed are treated as welcomed.
	if _, err := pool.Exec(ctx, `ALTER TABLE messenger_channels DROP COLUMN onboarded_at`); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !welcomed() {
		t.Fatal("channels that predate onboarding should be backfilled when the column is first created")
	}
}
