package postgres

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// FindPersonBySender finds the person who writes from a messenger account (see domain.Store).
func (s *Store) FindPersonBySender(ctx context.Context, identityID, senderID string) (string, error) {
	if senderID == "" {
		return "", domain.ErrNotFound
	}
	var person string
	err := s.pool.QueryRow(ctx, `
		SELECT mc.user_id
		FROM messenger_channels mc
		JOIN identities i ON i.id = mc.identity_id
		JOIN integrations g ON g.id = i.integration_id
		WHERE mc.sender_id = $2 AND (
			mc.identity_id = $1
			OR (g.service = ANY($3)
				AND g.service = (SELECT g2.service FROM identities i2 JOIN integrations g2 ON g2.id = i2.integration_id WHERE i2.id = $1)))
		ORDER BY (mc.identity_id = $1) DESC, mc.created_at NULLS LAST, mc.id
		LIMIT 1`, identityID, senderID, domain.SharedSenderServices()).Scan(&person)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return resolveLive(ctx, s, person)
}

// resolveLive follows a person who was merged to the one who remains.
func resolveLive(ctx context.Context, s *Store, id string) (string, error) {
	p, err := s.GetPerson(ctx, id)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// DuplicatePeople finds persons who are one human (see domain.Store). Two persons are the same human
// when the same messenger account, on a service whose sender ids are shared across bots, is behind a
// channel of each. It is transitive: if A and B share one account and B and C another, all three are one.
func (s *Store) DuplicatePeople(ctx context.Context) ([]domain.DuplicatePeople, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.service, mc.sender_id, mc.user_id
		FROM messenger_channels mc
		JOIN identities i ON i.id = mc.identity_id
		JOIN integrations g ON g.id = i.integration_id
		WHERE mc.sender_id <> '' AND g.service = ANY($1)`, domain.SharedSenderServices())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type account struct{ service, sender string }
	byAccount := map[account]map[string]bool{}
	for rows.Next() {
		var service, sender, person string
		if err := rows.Scan(&service, &sender, &person); err != nil {
			return nil, err
		}
		k := account{service, sender}
		if byAccount[k] == nil {
			byAccount[k] = map[string]bool{}
		}
		byAccount[k][person] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Union-find over persons that share an account.
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" {
			parent[x] = x
		}
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	via := map[string]map[string]bool{} // root → services
	type link struct {
		service string
		people  []string
	}
	var links []link
	for k, people := range byAccount {
		if len(people) < 2 {
			continue
		}
		var ids []string
		for p := range people {
			ids = append(ids, p)
		}
		sort.Strings(ids)
		for _, p := range ids[1:] {
			parent[find(p)] = find(ids[0])
		}
		links = append(links, link{k.service, ids})
	}
	for _, l := range links {
		root := find(l.people[0])
		if via[root] == nil {
			via[root] = map[string]bool{}
		}
		via[root][l.service] = true
	}
	groups := map[string][]string{}
	for p := range parent {
		groups[find(p)] = append(groups[find(p)], p)
	}

	var out []domain.DuplicatePeople
	for root, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		g := domain.DuplicatePeople{}
		for _, m := range members {
			dm := domain.DuplicateMember{PersonID: m}
			if err := s.pool.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM conversation_messages WHERE user_id = $1),
				       (SELECT count(*) FROM messenger_channels WHERE user_id = $1),
				       EXISTS (SELECT 1 FROM accounts WHERE person_id = $1 AND email_verified)`, m).
				Scan(&dm.Messages, &dm.Channels, &dm.VerifiedEmail); err != nil {
				return nil, err
			}
			g.Members = append(g.Members, dm)
		}
		g.Survivor = chooseSurvivor(g.Members)
		for svc := range via[root] {
			g.Via = append(g.Via, svc)
		}
		sort.Strings(g.Via)
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Members[0].PersonID < out[j].Members[0].PersonID })
	return out, nil
}

// chooseSurvivor picks who stays: someone with a verified email (the person has proved who they are),
// then whoever has said the most, then whoever writes from the most chats, then the lowest id so the
// choice is the same every time.
func chooseSurvivor(members []domain.DuplicateMember) string {
	best := members[0]
	better := func(a, b domain.DuplicateMember) bool {
		switch {
		case a.VerifiedEmail != b.VerifiedEmail:
			return a.VerifiedEmail
		case a.Messages != b.Messages:
			return a.Messages > b.Messages
		case a.Channels != b.Channels:
			return a.Channels > b.Channels
		}
		return a.PersonID < b.PersonID
	}
	for _, m := range members[1:] {
		if better(m, best) {
			best = m
		}
	}
	return best.PersonID
}

// MergeDuplicates folds every member of a group into its survivor.
func (s *Store) MergeDuplicates(ctx context.Context, g domain.DuplicatePeople) error {
	for _, m := range g.Members {
		if m.PersonID == g.Survivor {
			continue
		}
		if err := s.MergePersons(ctx, m.PersonID, g.Survivor); err != nil {
			return err
		}
	}
	return nil
}
