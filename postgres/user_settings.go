package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// UserTimezone returns the person's IANA time zone name, or "" if they haven't said.
func (s *Store) UserTimezone(ctx context.Context, userID string) (string, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT timezone FROM persons WHERE id = $1`, userID).Scan(&tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrNotFound
		}
		return "", err
	}
	return tz, nil
}

// SetUserTimezone records the person's IANA time zone name. The caller validates it.
func (s *Store) SetUserTimezone(ctx context.Context, userID, timezone string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE persons SET timezone = $2 WHERE id = $1`, userID, timezone)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetUserName records what the person wants to be called.
func (s *Store) SetUserName(ctx context.Context, userID, firstName, lastName string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE persons SET first_name = $2, last_name = $3 WHERE id = $1`, userID, firstName, lastName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// UserDetails returns what the person has told us about themselves (empty fields when
// they haven't said). The name is the first name they gave.
func (s *Store) UserDetails(ctx context.Context, userID string) (domain.UserDetails, error) {
	var d domain.UserDetails
	err := s.pool.QueryRow(ctx,
		`SELECT first_name, timezone, language, country, city FROM persons WHERE id = $1`, userID,
	).Scan(&d.Name, &d.Timezone, &d.Language, &d.Country, &d.City)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.UserDetails{}, domain.ErrNotFound
	}
	return d, err
}

// SetUserLanguage records the language to speak to the person in. The caller validates it.
func (s *Store) SetUserLanguage(ctx context.Context, userID, language string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE persons SET language = $2 WHERE id = $1`, userID, language)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetUserResidence records where the person lives. The caller validates the country.
func (s *Store) SetUserResidence(ctx context.Context, userID, country, city string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE persons SET country = $2, city = $3 WHERE id = $1`, userID, country, city)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
