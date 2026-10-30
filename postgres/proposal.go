package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

var _ domain.ProposalStore = (*Store)(nil)

const proposalCols = `id, person_id, agent_id, identity_id, chat_id, thread_id, message_id, text, choices, created_at, expires_at, chosen, chosen_at`

func scanProposal(row pgx.Row) (*domain.Proposal, error) {
	var p domain.Proposal
	var raw []byte
	if err := row.Scan(&p.ID, &p.PersonID, &p.AgentID, &p.IdentityID, &p.ChatID, &p.ThreadID, &p.MessageID, &p.Text, &raw, &p.CreatedAt, &p.ExpiresAt, &p.Chosen, &p.ChosenAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &p.Choices); err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveProposal keeps a proposal (or updates one: the message id is only known once the message is sent).
func (s *Store) SaveProposal(ctx context.Context, p *domain.Proposal) error {
	raw, err := json.Marshal(p.Choices)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO proposals (id, person_id, agent_id, identity_id, chat_id, thread_id, message_id, text, choices, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET message_id = EXCLUDED.message_id, text = EXCLUDED.text, choices = EXCLUDED.choices, expires_at = EXCLUDED.expires_at`,
		p.ID, p.PersonID, p.AgentID, p.IdentityID, p.ChatID, p.ThreadID, p.MessageID, p.Text, raw, p.ExpiresAt)
	return err
}

// GetProposal returns a proposal, or ErrNotFound.
func (s *Store) GetProposal(ctx context.Context, id string) (*domain.Proposal, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM proposals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return p, err
}

// ChooseProposal records that button i was tapped, once and only once (see domain.ProposalStore).
func (s *Store) ChooseProposal(ctx context.Context, id string, i int, now time.Time) (*domain.Proposal, error) {
	// One statement: of any number of taps at once, only one finds chosen still empty.
	p, err := scanProposal(s.pool.QueryRow(ctx, `
		UPDATE proposals SET chosen = $2, chosen_at = $3
		WHERE id = $1 AND chosen IS NULL AND expires_at > $3 AND $2 >= 0 AND $2 < jsonb_array_length(choices)
		RETURNING `+proposalCols, id, i, now))
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	// Nothing was chosen: say why.
	cur, gerr := s.GetProposal(ctx, id)
	switch {
	case gerr != nil:
		return nil, gerr
	case i < 0 || i >= len(cur.Choices):
		return nil, domain.ErrNotFound
	case cur.Chosen != nil:
		return nil, domain.ErrProposalChosen
	default:
		return nil, domain.ErrProposalExpired
	}
}

// DeleteExpiredProposals removes proposals that expired before the given time.
func (s *Store) DeleteExpiredProposals(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM proposals WHERE expires_at < $1`, before)
	return int(tag.RowsAffected()), err
}

var _ domain.ProposalCloser = (*Store)(nil)

// CloseProposal ends choices nobody chose (domain.ProposalCloser): they are no longer open.
func (s *Store) CloseProposal(ctx context.Context, id string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE proposals SET expires_at = LEAST(expires_at, $2) WHERE id = $1 AND chosen IS NULL`, id, now)
	return err
}

// LatestOpenProposal is the newest choices still open in a chat (domain.OpenProposalFinder).
func (s *Store) LatestOpenProposal(ctx context.Context, agentID, identityID, chatID string, now time.Time) (*domain.Proposal, error) {
	p, err := scanProposal(s.pool.QueryRow(ctx, `SELECT `+proposalCols+` FROM proposals
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND chosen IS NULL AND expires_at > $4
		ORDER BY created_at DESC LIMIT 1`, agentID, identityID, chatID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return p, err
}
