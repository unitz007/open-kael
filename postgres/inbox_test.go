package postgres_test

import (
	"context"
	"testing"

	"github.com/unitz007/open-kael/domain"
	"github.com/unitz007/open-kael/postgres/pgtest"
)

// What arrives is stored once: the same delivery again (GitHub redelivers) is the same row, and different ones are not
// mixed up. The facts come back whole.
func TestInbox_StoresADeliveryOnce(t *testing.T) {
	ctx := context.Background()
	store, pool := pgtest.Store(t)

	occ := func(delivery string) *domain.Occurrence {
		return &domain.Occurrence{Source: "inboxtest", ConnectionID: "c1", DeliveryID: delivery, Event: "inboxtest.thing.happened",
			Facts: map[string]any{"title": "Fix it", "number": 42}, SubjectKind: "inboxtest_account", SubjectValue: "octo", RepeatKey: delivery}
	}
	id, inserted, err := store.Insert(ctx, occ("d1"))
	if err != nil || !inserted || id == 0 {
		t.Fatalf("first delivery is stored: id=%d inserted=%v err=%v", id, inserted, err)
	}
	if _, again, err := store.Insert(ctx, occ("d1")); err != nil || again {
		t.Fatalf("the same delivery again is not stored twice: inserted=%v err=%v", again, err)
	}
	if _, other, err := store.Insert(ctx, occ("d2")); err != nil || !other {
		t.Fatalf("a different delivery is stored: inserted=%v err=%v", other, err)
	}
	var facts, subject string
	if err := pool.QueryRow(ctx, `SELECT facts::text, subject_value FROM events_inbox WHERE id = $1`, id).Scan(&facts, &subject); err != nil {
		t.Fatal(err)
	}
	if subject != "octo" || facts == "" {
		t.Errorf("facts and subject come back: %q %q", facts, subject)
	}
	counts, err := store.Counts(ctx)
	if err != nil || counts["new"] < 2 {
		t.Errorf("both are waiting: %v %v", counts, err)
	}
}
