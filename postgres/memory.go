package postgres

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/unitz007/open-kael/domain"
)

const (
	// memoryReadRows is how many of the latest stored messages are read, and memorySaid how many messages of words
	// (the person's and the minion's replies) a minion is given of them (domain.RecentWindow).
	memoryReadRows        = 80
	memorySaid            = 10
	memoryMaxContentBytes = 4000
	memoryContentOverflow = "[content too large — omitted]"
)

var _ domain.Memory = (*Store)(nil)

func (s *Store) History(ctx context.Context, key domain.ConversationKey) []domain.Message {
	if key.UserID == "" {
		return nil
	}

	// Memory is always user-scoped. Within a Slack thread (ThreadID set) we
	// narrow to that thread so replies stay in context.
	var (
		rows pgx.Rows
		err  error
	)
	if key.ThreadID != "" {
		rows, err = s.pool.Query(ctx, `
			SELECT role, content, tool_calls, tool_call_id, name
			FROM conversation_messages
			WHERE agent_id = $1 AND user_id = $2 AND thread_id = $3
			ORDER BY id DESC
			LIMIT $4
		`, key.AgentID, key.UserID, key.ThreadID, memoryReadRows)
	} else {
		rows, err = s.pool.Query(ctx, `
			SELECT role, content, tool_calls, tool_call_id, name
			FROM conversation_messages
			WHERE agent_id = $1 AND user_id = $2
			ORDER BY id DESC
			LIMIT $3
		`, key.AgentID, key.UserID, memoryReadRows)
	}
	if err != nil {
		log.Printf("memory: history agent=%s user=%s: %v", key.AgentID, key.UserID, err)
		return nil
	}
	defer rows.Close()

	var reversed []domain.Message
	for rows.Next() {
		var (
			role       string
			content    string
			toolJSON   []byte
			toolCallID string
			name       string
		)
		if err := rows.Scan(&role, &content, &toolJSON, &toolCallID, &name); err != nil {
			log.Printf("memory: scan agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
			continue
		}
		var calls []domain.ToolCall
		if len(toolJSON) > 2 {
			_ = json.Unmarshal(toolJSON, &calls)
		}
		reversed = append(reversed, domain.Message{
			Role:       domain.Role(role),
			Content:    content,
			ToolCalls:  calls,
			ToolCallID: toolCallID,
			Name:       name,
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("memory: rows agent=%s user=%s: %v", key.AgentID, key.UserID, err)
	}

	msgs := make([]domain.Message, len(reversed))
	for i, msg := range reversed {
		msgs[len(reversed)-1-i] = msg
	}
	return domain.RecentWindow(msgs, memorySaid)
}

func (s *Store) Append(ctx context.Context, key domain.ConversationKey, messages ...domain.Message) {
	if len(messages) == 0 {
		return
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (agent_id, identity_id, chat_id, thread_id, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (agent_id, identity_id, chat_id, thread_id) DO UPDATE SET updated_at = NOW()
	`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID)
	if err != nil {
		log.Printf("memory: upsert conversation agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
		return
	}

	for _, msg := range messages {
		content := msg.Content
		if len(content) > memoryMaxContentBytes {
			content = memoryContentOverflow
		}

		toolJSON := []byte("[]")
		if len(msg.ToolCalls) > 0 {
			if b, err := json.Marshal(msg.ToolCalls); err == nil {
				toolJSON = b
			}
		}

		_, err := s.pool.Exec(ctx, `
			INSERT INTO conversation_messages
				(agent_id, identity_id, chat_id, thread_id, user_id, role, content, tool_calls, tool_call_id, name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, key.UserID,
			string(msg.Role), content, toolJSON, msg.ToolCallID, msg.Name)
		if err != nil {
			log.Printf("memory: insert agent=%s chat=%s role=%s: %v", key.AgentID, key.ChatID, msg.Role, err)
		}
	}
}

// LastSkills is which capabilities the conversation's latest message was routed to, and when (domain.FollowUps).
func (s *Store) LastSkills(ctx context.Context, key domain.ConversationKey) ([]string, time.Time) {
	var skills []string
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT last_skills, last_skills_at FROM conversations
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4`,
		key.AgentID, key.IdentityID, key.ChatID, key.ThreadID).Scan(&skills, &at)
	if err != nil || at == nil {
		return nil, time.Time{}
	}
	return skills, *at
}

// SetLastSkills keeps which capabilities the conversation's latest message was routed to.
func (s *Store) SetLastSkills(ctx context.Context, key domain.ConversationKey, skills []string) {
	if len(skills) == 0 {
		return
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (agent_id, identity_id, chat_id, thread_id, updated_at, last_skills, last_skills_at)
		VALUES ($1, $2, $3, $4, NOW(), $5, NOW())
		ON CONFLICT (agent_id, identity_id, chat_id, thread_id) DO UPDATE SET last_skills = $5, last_skills_at = NOW()
	`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, skills)
	if err != nil {
		log.Printf("memory: last skills agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
	}
}

var _ domain.Exchanges = (*Store)(nil)

// LastExchange is the conversation's latest message from the person and the reply to it (domain.Exchanges).
func (s *Store) LastExchange(ctx context.Context, key domain.ConversationKey) (domain.LastExchange, bool) {
	var e domain.LastExchange
	var at *time.Time
	err := s.pool.QueryRow(ctx, `SELECT last_in_id, last_in_text, last_in_at, last_reply_id, last_changed FROM conversations
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4`,
		key.AgentID, key.IdentityID, key.ChatID, key.ThreadID).Scan(&e.MessageID, &e.Text, &at, &e.ReplyID, &e.Changed)
	if err != nil || at == nil || e.MessageID == "" {
		return domain.LastExchange{}, false
	}
	e.At = *at
	return e, true
}

// SetLastExchange keeps the conversation's latest message from the person and the reply to it.
func (s *Store) SetLastExchange(ctx context.Context, key domain.ConversationKey, e domain.LastExchange) {
	if e.Changed == nil {
		e.Changed = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (agent_id, identity_id, chat_id, thread_id, updated_at, last_in_id, last_in_text, last_in_at, last_reply_id, last_changed)
		VALUES ($1, $2, $3, $4, NOW(), $5, $6, $7, $8, $9)
		ON CONFLICT (agent_id, identity_id, chat_id, thread_id) DO UPDATE SET
			last_in_id = $5, last_in_text = $6, last_in_at = $7, last_reply_id = $8, last_changed = $9`,
		key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, e.MessageID, e.Text, e.At, e.ReplyID, e.Changed)
	if err != nil {
		log.Printf("memory: last exchange agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
	}
}

var _ domain.EditNotes = (*Store)(nil)

// MayNoteIgnoredEdit says whether the conversation may be told now that an edit was left alone, and marks it told
// (domain.EditNotes): once in domain.EditNoteEvery, decided in one step.
func (s *Store) MayNoteIgnoredEdit(ctx context.Context, key domain.ConversationKey) bool {
	tag, err := s.pool.Exec(ctx, `UPDATE conversations SET edit_note_at = NOW()
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4
		AND (edit_note_at IS NULL OR edit_note_at < NOW() - make_interval(secs => $5))`,
		key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, domain.EditNoteEvery.Seconds())
	if err != nil {
		log.Printf("memory: edit note agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
		return false
	}
	return tag.RowsAffected() == 1
}

// RewindLastExchange forgets the conversation's messages from the person's latest on (an edit's new answer takes
// their place), and the exchange itself.
func (s *Store) RewindLastExchange(ctx context.Context, key domain.ConversationKey) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM conversation_messages WHERE agent_id = $1 AND user_id = $2 AND chat_id = $3 AND thread_id = $4
		AND id >= (SELECT COALESCE(MAX(id), 0) FROM conversation_messages
			WHERE agent_id = $1 AND user_id = $2 AND chat_id = $3 AND thread_id = $4 AND role = 'user')
		AND EXISTS (SELECT 1 FROM conversation_messages
			WHERE agent_id = $1 AND user_id = $2 AND chat_id = $3 AND thread_id = $4 AND role = 'user')`,
		key.AgentID, key.UserID, key.ChatID, key.ThreadID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE conversations SET last_in_id = '', last_in_text = '', last_in_at = NULL, last_reply_id = '', last_changed = '{}'
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID)
	return err
}
