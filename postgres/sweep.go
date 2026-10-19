package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// personTraces are the tables where a person leaves a trace worth keeping. A person with a row in
// any of them, or with a name, time zone or other detail they gave, is not empty. (The notes tables
// count only when a note has something in it; a sorted-note mark or a refused value means nothing
// without the rest.)
var personTraces = []string{
	"messenger_channels", "conversation_messages", "user_channels", "email_verifications",
	"app_authorizations", "user_agent_configs", "user_tool_approvals", "user_app_instructions",
	"pending_turns", "profile_details",
}

// DeleteEmptyPeople removes people who have nothing: no chat, no history, no login, no connection,
// no settings, no notes, no details, nothing they told us, nobody merged into them, and nothing in the
// tables other packages registered. They are what is left when the bot someone wrote to is deleted
// (its channels go with it, the person does not), or when a first contact fails half way.
//
// A person made less than grace ago is left alone, so someone in the middle of a first message is
// never caught. It returns how many people were removed.
func (s *Store) DeleteEmptyPeople(ctx context.Context, grace time.Duration) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	extras, err := existingTables(ctx, tx, s.extraPersonTables())
	if err != nil {
		return 0, err
	}
	var none []string
	for _, t := range personTraces {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, t).Scan(&exists); err != nil {
			return 0, err
		}
		if exists {
			none = append(none, fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM %s WHERE user_id = p.id)`, t))
		}
	}
	for _, t := range extras {
		none = append(none, fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM %s WHERE %s = p.id)`, t.table, t.column))
	}
	none = append(none,
		`NOT EXISTS (SELECT 1 FROM accounts WHERE person_id = p.id)`,
		`NOT EXISTS (SELECT 1 FROM user_profiles WHERE user_id = p.id AND btrim(notes) <> '')`,
		`NOT EXISTS (SELECT 1 FROM user_integration_notes WHERE user_id = p.id AND btrim(notes) <> '')`,
		`NOT EXISTS (SELECT 1 FROM persons m WHERE m.merged_into = p.id)`,
	)

	// FOR UPDATE SKIP LOCKED: a person something is being added to at this very moment is skipped.
	tag, err := tx.Exec(ctx, `
		DELETE FROM persons WHERE id IN (
			SELECT p.id FROM persons p
			WHERE p.merged_into IS NULL
			  AND p.created_at < $1
			  AND p.first_name = '' AND p.last_name = '' AND p.timezone = ''
			  AND p.language = '' AND p.country = '' AND p.city = ''
			  AND `+strings.Join(none, "\n			  AND ")+`
			FOR UPDATE OF p SKIP LOCKED
		)`, time.Now().Add(-grace))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
