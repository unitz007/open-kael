package postgres

import (
	"context"
	"time"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.ProfileDetailStore = (*Store)(nil)

// ProfileDetails returns a person's details in the given scopes (every scope when none is given).
func (s *Store) ProfileDetails(ctx context.Context, userID string, scopes ...string) ([]domain.ProfileDetail, error) {
	q := `SELECT scope, key, value, source, created_at, updated_at, confirmed_at FROM profile_details WHERE user_id = $1`
	args := []any{userID}
	if len(scopes) > 0 {
		q += ` AND scope = ANY($2)`
		args = append(args, scopes)
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY scope, key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ProfileDetail
	for rows.Next() {
		var d domain.ProfileDetail
		if err := rows.Scan(&d.Scope, &d.Key, &d.Value, &d.Source, &d.CreatedAt, &d.UpdatedAt, &d.ConfirmedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetProfileDetail saves a detail, replacing any value held under the same key. A new key is refused
// with ErrProfileFull when the map already holds as many details as it may.
func (s *Store) SetProfileDetail(ctx context.Context, userID string, d domain.ProfileDetail) error {
	if d.Source == "" {
		d.Source = domain.DetailSaid
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO profile_details (user_id, scope, key, value, source)
		SELECT $1, $2, $3, $4, $5
		WHERE (SELECT count(*) FROM profile_details WHERE user_id = $1 AND scope = $2) < $6
		   OR EXISTS (SELECT 1 FROM profile_details WHERE user_id = $1 AND scope = $2 AND key = $3)
		ON CONFLICT (user_id, scope, key) DO UPDATE
		   SET value = EXCLUDED.value, source = EXCLUDED.source, updated_at = now(), confirmed_at = now()`,
		userID, d.Scope, d.Key, d.Value, d.Source, domain.MaxDetailsPerScope)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrProfileFull
	}
	return nil
}

// ConfirmProfileDetail notes that a detail was just confirmed to still be right.
func (s *Store) ConfirmProfileDetail(ctx context.Context, userID, scope, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE profile_details SET confirmed_at = now() WHERE user_id = $1 AND scope = $2 AND key = $3`, userID, scope, key)
	return err
}

// RemoveProfileDetail deletes one detail; it reports whether it was there.
func (s *Store) RemoveProfileDetail(ctx context.Context, userID, scope, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM profile_details WHERE user_id = $1 AND scope = $2 AND key = $3`, userID, scope, key)
	return tag.RowsAffected() == 1, err
}

// DeclineProfileValue remembers that a value was proposed for a detail and refused.
func (s *Store) DeclineProfileValue(ctx context.Context, userID, scope, key, value string) error {
	norm := domain.ComparableValue(value)
	if norm == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO profile_declined (user_id, scope, key, value_norm) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, scope, key, value_norm) DO UPDATE SET declined_at = now()`,
		userID, scope, key, norm)
	return err
}

// DeclinedProfileValue reports whether this value (compared loosely) was refused since the given time.
func (s *Store) DeclinedProfileValue(ctx context.Context, userID, scope, key, value string, since time.Time) (bool, error) {
	norm := domain.ComparableValue(value)
	if norm == "" {
		return false, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM profile_declined
		WHERE user_id = $1 AND scope = $2 AND key = $3 AND value_norm = $4 AND declined_at >= $5`,
		userID, scope, key, norm, since).Scan(&n)
	return n > 0, err
}

// ProfileMigrated reports whether a person's older note for a scope has been moved into details.
func (s *Store) ProfileMigrated(ctx context.Context, userID, scope string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM profile_migrations WHERE user_id = $1 AND scope = $2`, userID, scope).Scan(&n)
	return n > 0, err
}

// MarkProfileMigrated records that the move has been done.
func (s *Store) MarkProfileMigrated(ctx context.Context, userID, scope string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO profile_migrations (user_id, scope) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, scope)
	return err
}
