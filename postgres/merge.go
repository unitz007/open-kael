package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/unitz007/open-kael/domain"
)

// Everything that hangs on a person, and so moves when two people turn out to be one. A row that
// would collide with one the survivor already has (same key) is dropped: the survivor's wins.
//
// The tables listed here have no uniqueness that involves the person, so all their rows move at once.
var bulkPersonTables = []personTable{
	{"messenger_channels", "user_id"},
	{"conversation_messages", "user_id"},
	{"user_channels", "user_id"},
	{"email_verifications", "user_id"},
}

// These do have such uniqueness (one setting per person and key), so each row moves on its own.
var keyedPersonTables = []personTable{
	{"app_authorizations", "user_id"},
	{"user_agent_configs", "user_id"},
	{"user_tool_approvals", "user_id"},
	{"user_app_instructions", "user_id"},
	{"pending_turns", "user_id"},
	{"profile_details", "user_id"},
	{"profile_declined", "user_id"},
	{"profile_migrations", "user_id"},
	{"user_profiles", "user_id"},
	{"user_integration_notes", "user_id"},
}

type personTable struct{ table, column string }

var identRE = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// personTables is the extra tables registered on a store (see RegisterPersonTable).
type personTables struct {
	mu   sync.RWMutex
	list []personTable
}

// RegisterPersonTable tells the store about a table another package keeps, with a column holding a
// person's id, so that merging two people (and deleting one) moves or removes its rows too. Rows
// that collide on a unique key with the survivor's are dropped.
func (s *Store) RegisterPersonTable(table, column string) error {
	if !identRE.MatchString(table) || !identRE.MatchString(column) {
		return fmt.Errorf("postgres: not a table and column name: %q, %q", table, column)
	}
	s.extra.mu.Lock()
	defer s.extra.mu.Unlock()
	for _, t := range s.extra.list {
		if t.table == table && t.column == column {
			return nil
		}
	}
	s.extra.list = append(s.extra.list, personTable{table, column})
	return nil
}

func (s *Store) extraPersonTables() []personTable {
	s.extra.mu.RLock()
	defer s.extra.mu.RUnlock()
	return append([]personTable(nil), s.extra.list...)
}

// MergePersons folds one person into another that is the same human (see domain.Store).
func (s *Store) MergePersons(ctx context.Context, fromID, toID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if toID, err = resolvePersonTx(ctx, tx, toID); err != nil {
		return err
	}
	if err := mergePersonsTx(ctx, tx, s, fromID, toID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func mergePersonsTx(ctx context.Context, tx pgx.Tx, s *Store, fromID, toID string) error {
	if fromID == "" || toID == "" || fromID == toID {
		return fmt.Errorf("postgres: merge persons: need two different people (%q, %q)", fromID, toID)
	}
	// Lock both, always in the same order, so two merges cannot wait on each other.
	first, second := fromID, toID
	if second < first {
		first, second = second, first
	}
	for _, id := range []string{first, second} {
		var merged *string
		if err := tx.QueryRow(ctx, `SELECT merged_into FROM persons WHERE id = $1 FOR UPDATE`, id).Scan(&merged); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		if id == fromID && merged != nil {
			return fmt.Errorf("postgres: merge persons: %s was already merged into %s", fromID, *merged)
		}
	}

	// What they have told us about themselves: the survivor's value stays, gaps are filled.
	if _, err := tx.Exec(ctx, `
		UPDATE persons t SET
			first_name = CASE WHEN t.first_name = '' THEN f.first_name ELSE t.first_name END,
			last_name  = CASE WHEN t.last_name  = '' THEN f.last_name  ELSE t.last_name  END,
			timezone   = CASE WHEN t.timezone   = '' THEN f.timezone   ELSE t.timezone   END,
			language   = CASE WHEN t.language   = '' THEN f.language   ELSE t.language   END,
			country    = CASE WHEN t.country    = '' THEN f.country    ELSE t.country    END,
			city       = CASE WHEN t.city       = '' THEN f.city       ELSE t.city       END
		FROM persons f WHERE t.id = $2 AND f.id = $1`, fromID, toID); err != nil {
		return err
	}

	// Older free-text notes are joined, not dropped. A note that was not yet sorted into details
	// makes the survivor's note unsorted again, so the whole of it is sorted.
	if err := mergeNotes(ctx, tx, fromID, toID); err != nil {
		return err
	}

	for _, t := range bulkPersonTables {
		if _, err := tx.Exec(ctx, `UPDATE `+t.table+` SET `+t.column+` = $2 WHERE `+t.column+` = $1`, fromID, toID); err != nil {
			return fmt.Errorf("postgres: merge persons: move %s: %w", t.table, err)
		}
	}
	for _, t := range append(append([]personTable(nil), keyedPersonTables...), s.extraPersonTables()...) {
		if err := moveKeyedRows(ctx, tx, t, fromID, toID); err != nil {
			return fmt.Errorf("postgres: merge persons: move %s: %w", t.table, err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE accounts SET person_id = $2 WHERE person_id = $1`, fromID, toID); err != nil {
		return err
	}
	// The merged person stays as a marker, emptied, and anyone already merged into them follows on.
	if _, err := tx.Exec(ctx, `UPDATE persons SET merged_into = $2 WHERE merged_into = $1`, fromID, toID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE persons SET merged_into = $2, first_name = '', last_name = '', timezone = '', language = '', country = '', city = '' WHERE id = $1`, fromID, toID)
	return err
}

// mergeNotes joins the older free-text notes of two people.
func mergeNotes(ctx context.Context, tx pgx.Tx, fromID, toID string) error {
	// The personal note.
	var fromNote string
	err := tx.QueryRow(ctx, `SELECT notes FROM user_profiles WHERE user_id = $1`, fromID).Scan(&fromNote)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if fromNote != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_profiles (user_id, notes) VALUES ($2, $1)
			ON CONFLICT (user_id) DO UPDATE SET notes = CASE
				WHEN btrim(user_profiles.notes) = '' THEN EXCLUDED.notes
				ELSE user_profiles.notes || E'\n' || EXCLUDED.notes END`, fromNote, toID); err != nil {
			return err
		}
		if err := reopenIfUnsorted(ctx, tx, fromID, toID, "personal"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_profiles WHERE user_id = $1`, fromID); err != nil {
		return err
	}

	// The note for each app.
	rows, err := tx.Query(ctx, `SELECT integration_id, notes FROM user_integration_notes WHERE user_id = $1 AND btrim(notes) <> ''`, fromID)
	if err != nil {
		return err
	}
	type note struct{ integration, text string }
	var notes []note
	for rows.Next() {
		var n note
		if err := rows.Scan(&n.integration, &n.text); err != nil {
			rows.Close()
			return err
		}
		notes = append(notes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range notes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_integration_notes (user_id, integration_id, notes) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, integration_id) DO UPDATE SET notes = CASE
				WHEN btrim(user_integration_notes.notes) = '' THEN EXCLUDED.notes
				ELSE user_integration_notes.notes || E'\n' || EXCLUDED.notes END`, toID, n.integration, n.text); err != nil {
			return err
		}
		if err := reopenIfUnsorted(ctx, tx, fromID, toID, n.integration); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `DELETE FROM user_integration_notes WHERE user_id = $1`, fromID)
	return err
}

// reopenIfUnsorted: when the merged person's note had not been sorted into details, the survivor's
// note (now holding it) is no longer fully sorted either.
func reopenIfUnsorted(ctx context.Context, tx pgx.Tx, fromID, toID, scope string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM profile_migrations WHERE user_id = $1 AND scope = $2`, fromID, scope).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already sorted: the note is only a backup
	}
	_, err := tx.Exec(ctx, `DELETE FROM profile_migrations WHERE user_id = $1 AND scope = $2`, toID, scope)
	return err
}

// moveKeyedRows moves a person's rows one by one; a row that collides with one the survivor already
// has is dropped, so the survivor's value stands.
func moveKeyedRows(ctx context.Context, tx pgx.Tx, t personTable, fromID, toID string) error {
	rows, err := tx.Query(ctx, `SELECT ctid::text FROM `+t.table+` WHERE `+t.column+` = $1`, fromID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		sp, err := tx.Begin(ctx) // a savepoint: a collision undoes only this row
		if err != nil {
			return err
		}
		_, err = sp.Exec(ctx, `UPDATE `+t.table+` SET `+t.column+` = $1 WHERE ctid = $2::tid`, toID, id)
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			if err := sp.Commit(ctx); err != nil {
				return err
			}
		case errors.As(err, &pgErr) && pgErr.Code == "23505":
			if rerr := sp.Rollback(ctx); rerr != nil {
				return rerr
			}
			if _, err := tx.Exec(ctx, `DELETE FROM `+t.table+` WHERE ctid = $1::tid`, id); err != nil {
				return err
			}
		default:
			_ = sp.Rollback(ctx)
			return err
		}
	}
	return nil
}
