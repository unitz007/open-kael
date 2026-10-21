package postgres

import (
	"context"
	"fmt"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.PeopleStore = (*Store)(nil)

// RegisterIdentityKind says that the account a person connects to a service (their external user id) is an identifier
// of this kind, so saving that connection links the person to it (see domain.PeopleStore). Services not registered
// are not linked.
func (s *Store) RegisterIdentityKind(service, kind string) {
	s.identityKinds.Store(service, kind)
}

func (s *Store) identityKind(service string) string {
	if v, ok := s.identityKinds.Load(service); ok {
		return v.(string)
	}
	return ""
}

// Link records that a person is known by an identifier on a connection.
func (s *Store) Link(ctx context.Context, id domain.ExternalIdentity) error {
	id.Value = domain.NormalizeIdentifier(id.Kind, id.Value)
	if id.ConnectionID == "" || id.Kind == "" || id.Value == "" || id.PersonID == "" {
		return fmt.Errorf("postgres: link identity: need a connection, kind, value and person")
	}
	var auth any
	if id.AuthID != "" {
		auth = id.AuthID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO external_identities (connection_id, kind, value, person_id, auth_id)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (connection_id, kind, value, person_id) DO UPDATE SET auth_id = COALESCE(EXCLUDED.auth_id, external_identities.auth_id)`,
		id.ConnectionID, id.Kind, id.Value, id.PersonID, auth)
	return err
}

// PeopleFor returns the people linked to an identifier on a connection.
func (s *Store) PeopleFor(ctx context.Context, connectionID, kind, value string) ([]string, error) {
	value = domain.NormalizeIdentifier(kind, value)
	if connectionID == "" || kind == "" || value == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT person_id FROM external_identities WHERE connection_id = $1 AND kind = $2 AND value = $3 ORDER BY person_id`,
		connectionID, kind, value)
	if err != nil {
		return nil, fmt.Errorf("postgres: people for identifier: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// syncExternalIdentity makes the identity row for a connected account match the account: replaced when its
// identifier or person changed, removed when it no longer has one. Only for services with a registered kind.
func (s *Store) syncExternalIdentity(ctx context.Context, a *domain.AppAuthorization) error {
	var service string
	err := s.pool.QueryRow(ctx, `
		SELECT g.service FROM identities i JOIN integrations g ON g.id = i.integration_id WHERE i.id = $1`, a.IdentityID).Scan(&service)
	if err != nil {
		return nil // an account on an identity we cannot place has nothing to link
	}
	kind := s.identityKind(service)
	if kind == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM external_identities WHERE auth_id = $1`, a.ID); err != nil {
		return err
	}
	if v := domain.NormalizeIdentifier(kind, a.ExternalUserID); v != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO external_identities (connection_id, kind, value, person_id, auth_id) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (connection_id, kind, value, person_id) DO UPDATE SET auth_id = EXCLUDED.auth_id`,
			a.IdentityID, kind, v, a.UserID, a.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// BackfillExternalIdentities links every connected account of a service that is not yet linked, using the kind
// registered for it. Safe to run again. It returns how many were linked.
func (s *Store) BackfillExternalIdentities(ctx context.Context, service string) (int, error) {
	kind := s.identityKind(service)
	if kind == "" {
		return 0, fmt.Errorf("postgres: backfill identities: no kind registered for %q", service)
	}
	fold := "btrim(a.external_user_id)"
	if domain.NormalizeIdentifier(kind, "A") == "a" {
		fold = "lower(btrim(a.external_user_id))"
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO external_identities (connection_id, kind, value, person_id, auth_id)
		SELECT a.identity_id, $2, `+fold+`, a.user_id, a.id
		FROM app_authorizations a
		JOIN identities i ON i.id = a.identity_id
		JOIN integrations g ON g.id = i.integration_id
		WHERE g.service = $1 AND btrim(a.external_user_id) <> ''
		ON CONFLICT (connection_id, kind, value, person_id) DO NOTHING`, service, kind)
	if err != nil {
		return 0, fmt.Errorf("postgres: backfill identities: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
