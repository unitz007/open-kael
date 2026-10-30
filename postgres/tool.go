package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

func (s *Store) SaveTool(ctx context.Context, t *domain.ToolDefinition) error {
	inputJSON, err := json.Marshal(t.InputSchema)
	if err != nil {
		return fmt.Errorf("postgres: encode tool %q input schema: %w", t.ID, err)
	}
	outputJSON, err := json.Marshal(t.OutputSchema)
	if err != nil {
		return fmt.Errorf("postgres: encode tool %q output schema: %w", t.ID, err)
	}

	prepared := t.Prepared
	if prepared == nil {
		prepared = []string{}
	}
	preparedInputs := t.PreparedInputs
	if preparedInputs == nil {
		preparedInputs = map[string]domain.Schema{}
	}
	first, firstAny, with, onceFor := t.First, t.FirstAny, t.With, t.OnceFor
	if first == nil {
		first = []string{}
	}
	if firstAny == nil {
		firstAny = []string{}
	}
	if with == nil {
		with = []string{}
	}
	if onceFor == nil {
		onceFor = []string{}
	}
	preparedJSON, err := json.Marshal(preparedInputs)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tools (id, name, function_name, description, instructions, integration_id, input_schema, output_schema, action, requires_approval, approval_prompt_template, approval_timeout_seconds, read_only, disabled, needs_local_time, prepared, max_calls, prepared_inputs, first_needed, comes_with, once_for, first_any)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			function_name = EXCLUDED.function_name,
			description = EXCLUDED.description,
			instructions = EXCLUDED.instructions,
			integration_id = EXCLUDED.integration_id,
			input_schema = EXCLUDED.input_schema,
			output_schema = EXCLUDED.output_schema,
			action = EXCLUDED.action,
			requires_approval = EXCLUDED.requires_approval,
			approval_prompt_template = EXCLUDED.approval_prompt_template,
			approval_timeout_seconds = EXCLUDED.approval_timeout_seconds,
			read_only = EXCLUDED.read_only,
			disabled = EXCLUDED.disabled,
			needs_local_time = EXCLUDED.needs_local_time,
			prepared = EXCLUDED.prepared,
			prepared_inputs = EXCLUDED.prepared_inputs,
			max_calls = EXCLUDED.max_calls,
			first_needed = EXCLUDED.first_needed,
			comes_with = EXCLUDED.comes_with,
			once_for = EXCLUDED.once_for,
			first_any = EXCLUDED.first_any
	`, t.ID, t.Name, t.FunctionName, t.Description, t.Instructions, t.IntegrationID, inputJSON, outputJSON, t.Action, t.RequiresApproval, t.ApprovalPromptTemplate, t.ApprovalTimeoutSeconds, t.ReadOnly, t.Disabled, t.NeedsLocalTime, prepared, t.MaxCalls, preparedJSON, first, with, onceFor, firstAny)
	if err != nil {
		return fmt.Errorf("postgres: save tool %q: %w", t.ID, err)
	}
	return nil
}

func (s *Store) GetTool(ctx context.Context, id string) (*domain.ToolDefinition, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, function_name, description, instructions, integration_id, input_schema, output_schema, action, requires_approval, approval_prompt_template, approval_timeout_seconds, read_only, disabled, needs_local_time, prepared, max_calls, prepared_inputs, first_needed, comes_with, once_for, first_any
		FROM tools WHERE id = $1
	`, id)
	t, err := scanTool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return t, err
}

func (s *Store) ListToolsByIntegration(ctx context.Context, integrationID string) ([]*domain.ToolDefinition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, function_name, description, instructions, integration_id, input_schema, output_schema, action, requires_approval, approval_prompt_template, approval_timeout_seconds, read_only, disabled, needs_local_time, prepared, max_calls, prepared_inputs, first_needed, comes_with, once_for, first_any
		FROM tools WHERE integration_id = $1 ORDER BY id
	`, integrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.ToolDefinition
	for rows.Next() {
		t, err := scanTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTool(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM tools WHERE id = $1`, id)
	return err
}

func scanTool(row rowScanner) (*domain.ToolDefinition, error) {
	var t domain.ToolDefinition
	var inputJSON, outputJSON, preparedJSON []byte
	if err := row.Scan(&t.ID, &t.Name, &t.FunctionName, &t.Description, &t.Instructions, &t.IntegrationID, &inputJSON, &outputJSON, &t.Action, &t.RequiresApproval, &t.ApprovalPromptTemplate, &t.ApprovalTimeoutSeconds, &t.ReadOnly, &t.Disabled, &t.NeedsLocalTime, &t.Prepared, &t.MaxCalls, &preparedJSON, &t.First, &t.With, &t.OnceFor, &t.FirstAny); err != nil {
		return nil, err
	}
	if len(preparedJSON) > 2 { // "{}" is none
		if err := json.Unmarshal(preparedJSON, &t.PreparedInputs); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(inputJSON, &t.InputSchema); err != nil {
		return nil, fmt.Errorf("postgres: decode tool %q input schema: %w", t.ID, err)
	}
	if err := json.Unmarshal(outputJSON, &t.OutputSchema); err != nil {
		return nil, fmt.Errorf("postgres: decode tool %q output schema: %w", t.ID, err)
	}
	return &t, nil
}
