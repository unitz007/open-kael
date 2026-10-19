package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

const personCols = `id, first_name, last_name, timezone, language, country, city, created_at, merged_into`

func scanPerson(row pgx.Row) (*domain.Person, error) {
	var p domain.Person
	var merged *string
	if err := row.Scan(&p.ID, &p.FirstName, &p.LastName, &p.Timezone, &p.Language, &p.Country, &p.City, &p.CreatedAt, &merged); err != nil {
		return nil, err
	}
	if merged != nil {
		p.MergedInto = *merged
	}
	return &p, nil
}

func (s *Store) SavePerson(ctx context.Context, p *domain.Person) error {
	var merged *string
	if p.MergedInto != "" {
		merged = &p.MergedInto
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO persons (id, first_name, last_name, timezone, language, country, city, merged_into)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			first_name  = EXCLUDED.first_name,
			last_name   = EXCLUDED.last_name,
			timezone    = EXCLUDED.timezone,
			language    = EXCLUDED.language,
			country     = EXCLUDED.country,
			city        = EXCLUDED.city,
			merged_into = EXCLUDED.merged_into
	`, p.ID, p.FirstName, p.LastName, p.Timezone, p.Language, p.Country, p.City, merged)
	return err
}

// GetPerson returns the person with this id. A person who was merged into another is followed to
// the one who remains, so an old id still leads to the right person.
func (s *Store) GetPerson(ctx context.Context, id string) (*domain.Person, error) {
	for i := 0; i < 16; i++ {
		p, err := scanPerson(s.pool.QueryRow(ctx, `SELECT `+personCols+` FROM persons WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if p.MergedInto == "" {
			return p, nil
		}
		id = p.MergedInto
	}
	return nil, fmt.Errorf("postgres: person merge chain too long")
}

// DeletePerson removes a person and everything that hangs on them.
func (s *Store) DeletePerson(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM conversation_messages WHERE user_id = $1`, id); err != nil {
		return err
	}
	extras, err := existingTables(ctx, tx, s.extraPersonTables())
	if err != nil {
		return err
	}
	for _, t := range extras {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t.table+` WHERE `+t.column+` = $1`, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM persons WHERE id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnsurePersonForAccount returns the person an account is for, creating and linking a new one the
// first time.
func (s *Store) EnsurePersonForAccount(ctx context.Context, accountID string) (*domain.Person, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var person *string
	err = tx.QueryRow(ctx, `SELECT person_id FROM accounts WHERE id = $1 FOR UPDATE`, accountID).Scan(&person)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if person != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return s.GetPerson(ctx, *person)
	}
	p := &domain.Person{ID: domain.NewID()}
	if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = $2 WHERE id = $1`, accountID, p.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetPerson(ctx, p.ID)
}

// LinkEmail attaches a verified email to a person (see domain.Store).
func (s *Store) LinkEmail(ctx context.Context, personID, email string) (string, error) {
	email = domain.NormalizeEmail(email)
	if email == "" {
		return "", fmt.Errorf("postgres: link email: no email")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var survivor string
	for attempt := 0; ; attempt++ {
		var id string
		var person *string
		var verified bool
		err := tx.QueryRow(ctx, `SELECT id, person_id, email_verified FROM accounts WHERE email = $1 FOR UPDATE`, email).Scan(&id, &person, &verified)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			tag, ierr := tx.Exec(ctx, `
				INSERT INTO accounts (id, email, email_verified, person_id) VALUES ($1, $2, TRUE, $3)
				ON CONFLICT (email) DO NOTHING`, domain.NewID(), email, personID)
			if ierr != nil {
				return "", ierr
			}
			if tag.RowsAffected() == 0 && attempt < 2 {
				continue // someone else made it a moment ago: read theirs
			}
			survivor = personID
		case err != nil:
			return "", err
		case !verified:
			// Whoever registered this login never proved the email is theirs; only the person who just
			// did has. So the login is theirs now, and what the registrant set (a password, a pending
			// token, signed-in sessions) is wiped, and nobody is merged into the registrant's person.
			if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = $2, email_verified = TRUE, password_hash = '', verification_token = '' WHERE id = $1`, id, personID); err != nil {
				return "", err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE account_id = $1`, id); err != nil {
				return "", err
			}
			survivor = personID
		case person == nil || *person == personID:
			if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = $2, email_verified = TRUE WHERE id = $1`, id, personID); err != nil {
				return "", err
			}
			survivor = personID
		default:
			other, err := resolvePersonTx(ctx, tx, *person)
			if err != nil {
				return "", err
			}
			survivor = other
			if other != personID {
				if err := mergePersonsTx(ctx, tx, s, personID, other); err != nil {
					return "", err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = $2, email_verified = TRUE WHERE id = $1`, id, survivor); err != nil {
				return "", err
			}
		}
		break
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return survivor, nil
}

// UnlinkEmail detaches every login from a person.
func (s *Store) UnlinkEmail(ctx context.Context, personID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// A login that is only a link (no password, owns nothing) has no use apart from the person.
	if _, err := tx.Exec(ctx, `
		DELETE FROM accounts a WHERE a.person_id = $1 AND a.password_hash = ''
		  AND NOT EXISTS (SELECT 1 FROM agents x WHERE x.created_by = a.id)
		  AND NOT EXISTS (SELECT 1 FROM integrations x WHERE x.created_by = a.id)
		  AND NOT EXISTS (SELECT 1 FROM identities x WHERE x.created_by = a.id)`, personID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = NULL WHERE person_id = $1`, personID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PersonHasVerifiedEmail reports whether a person has a login with a verified email.
func (s *Store) PersonHasVerifiedEmail(ctx context.Context, personID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE person_id = $1 AND email_verified)`, personID).Scan(&ok)
	return ok, err
}

// resolvePersonTx follows a merged person to the one who remains.
func resolvePersonTx(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	for i := 0; i < 16; i++ {
		var merged *string
		if err := tx.QueryRow(ctx, `SELECT merged_into FROM persons WHERE id = $1`, id).Scan(&merged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", domain.ErrNotFound
			}
			return "", err
		}
		if merged == nil {
			return id, nil
		}
		id = *merged
	}
	return "", fmt.Errorf("postgres: person merge chain too long")
}
