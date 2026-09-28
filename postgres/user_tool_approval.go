package postgres

import (
	"context"
)

// SetUserToolApproval upserts a per-user tool approval preference. Calling it
// with requires=true adds the row; requires=false removes it.
func (s *Store) SetUserToolApproval(ctx context.Context, userID, toolID string, requires bool) error {
	if requires {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO user_tool_approvals (user_id, tool_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			userID, toolID,
		)
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM user_tool_approvals WHERE user_id = $1 AND tool_id = $2`,
		userID, toolID,
	)
	return err
}

// ListUserToolApprovals returns the set of tool IDs the user has flagged for
// approval, as a map[toolID]bool ready for domain.WithUserToolApprovals.
func (s *Store) ListUserToolApprovals(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tool_id FROM user_tool_approvals WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var toolID string
		if err := rows.Scan(&toolID); err != nil {
			return nil, err
		}
		result[toolID] = true
	}
	return result, rows.Err()
}
