package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// SaveSkill upserts the skill row and replaces its skill_tools join rows
// to match skill.Tools exactly, in one transaction. skill.AgentID is the
// owning Agent — see domain.Skill's own doc comment on that field.
func (s *Store) SaveSkill(ctx context.Context, skill *domain.Skill) error {
	inputJSON, err := json.Marshal(skill.InputSchema)
	if err != nil {
		return fmt.Errorf("postgres: encode skill %q input schema: %w", skill.ID, err)
	}
	outputJSON, err := json.Marshal(skill.OutputSchema)
	if err != nil {
		return fmt.Errorf("postgres: encode skill %q output schema: %w", skill.ID, err)
	}

	var triggerType, triggerValue *string
	if skill.Trigger != nil {
		tt := string(skill.Trigger.Type)
		triggerType = &tt
		triggerValue = &skill.Trigger.Value
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		INSERT INTO skills (id, agent_id, name, description, instructions, input_schema, output_schema, visibility, schedulable, trigger_type, trigger_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			agent_id = EXCLUDED.agent_id,
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			instructions = EXCLUDED.instructions,
			input_schema = EXCLUDED.input_schema,
			output_schema = EXCLUDED.output_schema,
			visibility = EXCLUDED.visibility,
			schedulable = EXCLUDED.schedulable,
			trigger_type = EXCLUDED.trigger_type,
			trigger_value = EXCLUDED.trigger_value
	`, skill.ID, skill.AgentID, skill.Name, skill.Description, skill.Instructions,
		inputJSON, outputJSON, string(skill.Visibility), skill.Schedulable, triggerType, triggerValue)
	if err != nil {
		return fmt.Errorf("postgres: save skill %q: %w", skill.ID, err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM skill_tools WHERE skill_id = $1`, skill.ID); err != nil {
		return fmt.Errorf("postgres: clear skill %q tool bindings: %w", skill.ID, err)
	}
	for _, binding := range skill.Tools {
		if _, err := tx.Exec(ctx, `INSERT INTO skill_tools (skill_id, tool_id) VALUES ($1, $2)`, skill.ID, binding.ToolID); err != nil {
			return fmt.Errorf("postgres: bind tool %q to skill %q: %w", binding.ToolID, skill.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit skill %q: %w", skill.ID, err)
	}
	return nil
}

func (s *Store) GetSkill(ctx context.Context, id string) (*domain.Skill, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, agent_id, name, description, instructions, input_schema, output_schema, visibility, schedulable, trigger_type, trigger_value
		FROM skills WHERE id = $1
	`, id)
	skill, err := scanSkill(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	tools, err := s.toolBindingsForSkill(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: load tool bindings for skill %q: %w", id, err)
	}
	skill.Tools = tools
	return skill, nil
}

func (s *Store) ListSkillsByAgent(ctx context.Context, agentID string) ([]*domain.Skill, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, agent_id, name, description, instructions, input_schema, output_schema, visibility, schedulable, trigger_type, trigger_value
		FROM skills WHERE agent_id = $1 ORDER BY id
	`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.Skill
	for rows.Next() {
		skill, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, skill)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, skill := range out {
		tools, err := s.toolBindingsForSkill(ctx, skill.ID)
		if err != nil {
			return nil, fmt.Errorf("postgres: load tool bindings for skill %q: %w", skill.ID, err)
		}
		skill.Tools = tools
	}
	return out, nil
}

func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM skills WHERE id = $1`, id)
	return err
}

func (s *Store) toolBindingsForSkill(ctx context.Context, skillID string) ([]domain.ToolBinding, error) {
	rows, err := s.pool.Query(ctx, `SELECT tool_id FROM skill_tools WHERE skill_id = $1 ORDER BY tool_id`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ToolBinding
	for rows.Next() {
		var toolID string
		if err := rows.Scan(&toolID); err != nil {
			return nil, err
		}
		out = append(out, domain.ToolBinding{ToolID: toolID})
	}
	return out, rows.Err()
}

func scanSkill(row rowScanner) (*domain.Skill, error) {
	var skill domain.Skill
	var inputJSON, outputJSON []byte
	var visibility string
	var triggerType, triggerValue *string

	if err := row.Scan(&skill.ID, &skill.AgentID, &skill.Name, &skill.Description, &skill.Instructions,
		&inputJSON, &outputJSON, &visibility, &skill.Schedulable, &triggerType, &triggerValue); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(inputJSON, &skill.InputSchema); err != nil {
		return nil, fmt.Errorf("postgres: decode skill %q input schema: %w", skill.ID, err)
	}
	if err := json.Unmarshal(outputJSON, &skill.OutputSchema); err != nil {
		return nil, fmt.Errorf("postgres: decode skill %q output schema: %w", skill.ID, err)
	}
	skill.Visibility = domain.SkillVisibility(visibility)
	if triggerType != nil {
		skill.Trigger = &domain.Trigger{Type: domain.TriggerType(*triggerType), Value: *triggerValue}
	}
	return &skill, nil
}
