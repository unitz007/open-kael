package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

func TestProfileDetails_SavedReplacedConfirmedRemoved(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"u_pd", "u_pd2"} {
		if err := s.SavePerson(ctx, &domain.Person{ID: id}); err != nil {
			t.Fatal(err)
		}
		id := id
		t.Cleanup(func() { _ = s.DeletePerson(ctx, id) })
	}
	set := func(user, scope, key, val, src string) {
		t.Helper()
		if err := s.SetProfileDetail(ctx, user, domain.ProfileDetail{Scope: scope, Key: key, Value: val, Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	set("u_pd", domain.ProfilePersonal, "residence", "Wolfsburg", domain.DetailSaid)
	set("u_pd", domain.ProfilePersonal, "name", "Charles Dinneya", domain.DetailSaid)
	set("u_pd", "int_fpl", "favourite_club", "Arsenal", domain.DetailSaid)
	set("u_pd2", domain.ProfilePersonal, "residence", "Lagos", domain.DetailSaid)

	got, err := s.ProfileDetails(ctx, "u_pd", domain.ProfilePersonal)
	if err != nil || len(got) != 2 || got[0].Key != "name" || got[1].Key != "residence" {
		t.Fatalf("one scope only, in order: %+v %v", got, err)
	}
	if all, _ := s.ProfileDetails(ctx, "u_pd"); len(all) != 3 {
		t.Errorf("every scope when none is named: %d", len(all))
	}
	if other, _ := s.ProfileDetails(ctx, "u_pd2"); len(other) != 1 || other[0].Value != "Lagos" {
		t.Errorf("people never see each other's details: %+v", other)
	}

	first := got[1]
	time.Sleep(20 * time.Millisecond)
	set("u_pd", domain.ProfilePersonal, "residence", "Wolfsburg, Germany", domain.DetailChanged)
	after, _ := s.ProfileDetails(ctx, "u_pd", domain.ProfilePersonal)
	r := after[1]
	if r.Value != "Wolfsburg, Germany" || r.Source != domain.DetailChanged || !r.CreatedAt.Equal(first.CreatedAt) || !r.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("replaced in place, keeping when it was first learned: %+v (was %+v)", r, first)
	}
	if after[0].Value != "Charles Dinneya" {
		t.Errorf("changing one detail leaves the others alone: %+v", after[0])
	}

	time.Sleep(20 * time.Millisecond)
	if err := s.ConfirmProfileDetail(ctx, "u_pd", domain.ProfilePersonal, "name"); err != nil {
		t.Fatal(err)
	}
	conf, _ := s.ProfileDetails(ctx, "u_pd", domain.ProfilePersonal)
	if !conf[0].ConfirmedAt.After(after[0].ConfirmedAt) || !conf[0].UpdatedAt.Equal(after[0].UpdatedAt) || conf[0].Value != "Charles Dinneya" {
		t.Errorf("confirming only refreshes the confirmed time: %+v", conf[0])
	}

	if ok, err := s.RemoveProfileDetail(ctx, "u_pd", domain.ProfilePersonal, "residence"); err != nil || !ok {
		t.Errorf("removed: %v %v", ok, err)
	}
	if ok, _ := s.RemoveProfileDetail(ctx, "u_pd", domain.ProfilePersonal, "residence"); ok {
		t.Error("removing what is not there reports so")
	}
	if ok, _ := s.RemoveProfileDetail(ctx, "u_pd2", domain.ProfilePersonal, "name"); ok {
		t.Error("removing someone else's detail does nothing")
	}
}

func TestProfileDetails_CapAndDeclinedAndMigrated(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SavePerson(ctx, &domain.Person{ID: "u_pc"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.DeletePerson(ctx, "u_pc") })

	for i := 0; i < domain.MaxDetailsPerScope; i++ {
		if err := s.SetProfileDetail(ctx, "u_pc", domain.ProfileDetail{Scope: "personal", Key: fmt.Sprintf("k%02d", i), Value: "v"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetProfileDetail(ctx, "u_pc", domain.ProfileDetail{Scope: "personal", Key: "one_more", Value: "v"}); !errors.Is(err, domain.ErrProfileFull) {
		t.Errorf("a full map takes no new detail: %v", err)
	}
	if err := s.SetProfileDetail(ctx, "u_pc", domain.ProfileDetail{Scope: "personal", Key: "k03", Value: "changed"}); err != nil {
		t.Errorf("but a detail it holds can still change: %v", err)
	}
	if err := s.SetProfileDetail(ctx, "u_pc", domain.ProfileDetail{Scope: "int_x", Key: "one_more", Value: "v"}); err != nil {
		t.Errorf("each map has its own room: %v", err)
	}

	since := time.Now().Add(-time.Hour)
	if got, _ := s.DeclinedProfileValue(ctx, "u_pc", "personal", "residence", "Berlin", since); got {
		t.Error("nothing declined yet")
	}
	if err := s.DeclineProfileValue(ctx, "u_pc", "personal", "residence", "Berlin, Germany"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DeclinedProfileValue(ctx, "u_pc", "personal", "residence", " berlin  germany ", since); !got {
		t.Error("the same value in other spelling is still declined")
	}
	if got, _ := s.DeclinedProfileValue(ctx, "u_pc", "personal", "residence", "Berlin", since); got {
		t.Error("a different value is not")
	}
	if got, _ := s.DeclinedProfileValue(ctx, "u_pc", "personal", "country", "Berlin, Germany", since); got {
		t.Error("declining for one detail says nothing about another")
	}
	if got, _ := s.DeclinedProfileValue(ctx, "u_pc", "personal", "residence", "Berlin, Germany", time.Now().Add(time.Hour)); got {
		t.Error("a refusal older than the cut-off no longer counts")
	}
	if err := s.DeclineProfileValue(ctx, "u_pc", "personal", "residence", "!!!"); err != nil {
		t.Errorf("nothing to remember is not an error: %v", err)
	}

	if m, _ := s.ProfileMigrated(ctx, "u_pc", "personal"); m {
		t.Error("not migrated yet")
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkProfileMigrated(ctx, "u_pc", "personal"); err != nil {
			t.Fatal(err)
		}
	}
	if m, _ := s.ProfileMigrated(ctx, "u_pc", "personal"); !m {
		t.Error("migrated")
	}
	if m, _ := s.ProfileMigrated(ctx, "u_pc", "int_x"); m {
		t.Error("per scope")
	}
}

func TestProfileDetails_GoWithThePerson(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SavePerson(ctx, &domain.Person{ID: "u_pg"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetProfileDetail(ctx, "u_pg", domain.ProfileDetail{Scope: "personal", Key: "name", Value: "X"})
	_ = s.DeclineProfileValue(ctx, "u_pg", "personal", "name", "Y")
	_ = s.MarkProfileMigrated(ctx, "u_pg", "personal")
	if err := s.DeletePerson(ctx, "u_pg"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.ProfileDetails(ctx, "u_pg"); len(d) != 0 {
		t.Errorf("deleting a person deletes what was learned: %+v", d)
	}
	if got, _ := s.DeclinedProfileValue(ctx, "u_pg", "personal", "name", "Y", time.Time{}); got {
		t.Error("and what they declined")
	}
	if m, _ := s.ProfileMigrated(ctx, "u_pg", "personal"); m {
		t.Error("and the migration mark")
	}
}
