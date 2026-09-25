package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

// SaveAgent persists the Agent row and its IdentityIDs join rows atomically.
// LLMs, Loop, and Directory are live objects (json:"-") and never touch the DB.
func (s *Store) SaveAgent(ctx context.Context, a *domain.Agent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	createdBy := &a.CreatedBy
	if a.CreatedBy == "" {
		createdBy = nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO agents (id, name, description, instructions, max_iterations, llm_model, llm_base_url, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			instructions = EXCLUDED.instructions,
			max_iterations = EXCLUDED.max_iterations,
			llm_model = EXCLUDED.llm_model,
			llm_base_url = EXCLUDED.llm_base_url
	`, a.ID, a.Name, a.Description, a.Instructions, a.MaxIterations, a.LLMConfig.Model, a.LLMConfig.BaseURL, createdBy)
	if err != nil {
		return err
	}

	if _, err = tx.Exec(ctx, `DELETE FROM agent_identities WHERE agent_id = $1`, a.ID); err != nil {
		return err
	}
	for _, identityID := range a.IdentityIDs {
		if _, err = tx.Exec(ctx, `
			INSERT INTO agent_identities (agent_id, identity_id) VALUES ($1, $2)
		`, a.ID, identityID); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// GetAgent returns the Agent's stored fields including IdentityIDs.
// Skills is left nil; see Store.LoadAgent to also populate it.
func (s *Store) GetAgent(ctx context.Context, id string) (*domain.Agent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, description, instructions, max_iterations, llm_model, llm_base_url, created_by
		FROM agents WHERE id = $1
	`, id)
	a, err := scanAgent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadIdentityIDs(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// ListAgents returns all agents. When ownerID is non-empty only agents created
// by that user are returned; pass "" to retrieve every agent (used at boot).
func (s *Store) ListAgents(ctx context.Context, ownerID string) ([]*domain.Agent, error) {
	var rows pgx.Rows
	var err error
	if ownerID != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT id, name, description, instructions, max_iterations, llm_model, llm_base_url, created_by
			FROM agents WHERE (created_by = $1 OR created_by IS NULL) ORDER BY id
		`, ownerID)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT id, name, description, instructions, max_iterations, llm_model, llm_base_url, created_by
			FROM agents ORDER BY id
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []*domain.Agent
	var ids []string
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
		ids = append(ids, a.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return agents, nil
	}

	// Batch-load all identity bindings in one query to avoid N+1.
	idRows, err := s.pool.Query(ctx, `
		SELECT agent_id, identity_id FROM agent_identities
		WHERE agent_id = ANY($1) ORDER BY agent_id, identity_id
	`, ids)
	if err != nil {
		return nil, err
	}
	defer idRows.Close()

	byAgentID := make(map[string]*domain.Agent, len(agents))
	for _, a := range agents {
		byAgentID[a.ID] = a
	}
	for idRows.Next() {
		var agentID, identityID string
		if err := idRows.Scan(&agentID, &identityID); err != nil {
			return nil, err
		}
		a := byAgentID[agentID]
		a.IdentityIDs = append(a.IdentityIDs, identityID)
	}
	return agents, idRows.Err()
}

// GetAgentByIdentityID returns the agent that has the given identity linked,
// or ErrNotFound when no agent uses that identity.
func (s *Store) GetAgentByIdentityID(ctx context.Context, identityID string) (*domain.Agent, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT a.id, a.name, a.description, a.instructions, a.max_iterations, a.llm_model, a.llm_base_url, a.created_by
		FROM agents a
		JOIN agent_identities ai ON ai.agent_id = a.id
		WHERE ai.identity_id = $1
		LIMIT 1
	`, identityID)
	a, err := scanAgent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadIdentityIDs(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Store) DeleteAgent(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, id)
	return err
}

func (s *Store) loadIdentityIDs(ctx context.Context, a *domain.Agent) error {
	rows, err := s.pool.Query(ctx, `
		SELECT identity_id FROM agent_identities WHERE agent_id = $1 ORDER BY identity_id
	`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		a.IdentityIDs = append(a.IdentityIDs, id)
	}
	return rows.Err()
}

func scanAgent(row rowScanner) (*domain.Agent, error) {
	var a domain.Agent
	var createdBy *string
	if err := row.Scan(&a.ID, &a.Name, &a.Description, &a.Instructions, &a.MaxIterations, &a.LLMConfig.Model, &a.LLMConfig.BaseURL, &createdBy); err != nil {
		return nil, err
	}
	if createdBy != nil {
		a.CreatedBy = *createdBy
	}
	return &a, nil
}
