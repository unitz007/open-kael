package postgres

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/unitz007/open-kael/domain"
)

const (
	// memoryReadRows is how many of the latest stored messages are read; of them a minion is given the last memoryWhole
	// exchanges whole and, before them, words, memorySaid messages of words in all (domain.RecentWindow).
	memoryReadRows        = 150
	memoryWhole           = 3
	memorySaid            = 10
	memoryMaxContentBytes = 4000
	memoryContentOverflow = "[content too large — omitted]"
)

var _ domain.Memory = (*Store)(nil)

func (s *Store) History(ctx context.Context, key domain.ConversationKey) []domain.Message {
	if key.UserID == "" {
		return nil
	}

	// One conversation per person and minion (decided 2026-10-31): everything the person said to this minion, on any
	// chat app, in any chat or thread, is one conversation. A Slack thread used to narrow it to that thread, so each new
	// message in a Slack direct message (its own thread) started a conversation with nothing in it.
	// In a chat others read (a Slack channel, key.Shared), only what was said in it.
	rows, err := s.pool.Query(ctx, `
		SELECT role, content, tool_calls, tool_call_id, name
		FROM conversation_messages
		WHERE agent_id = $1 AND user_id = $2 AND (NOT $4::bool OR (identity_id = $5 AND chat_id = $6))
		ORDER BY id DESC
		LIMIT $3
	`, key.AgentID, key.UserID, memoryReadRows, key.Shared, key.IdentityID, key.ChatID)
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
	return domain.RecentWindow(msgs, memoryWhole, memorySaid)
}

func (s *Store) Append(ctx context.Context, key domain.ConversationKey, messages ...domain.Message) {
	if len(messages) == 0 {
		return
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (agent_id, identity_id, chat_id, thread_id, updated_at, user_id)
		VALUES ($1, $2, $3, $4, NOW(), $5)
		ON CONFLICT (agent_id, identity_id, chat_id, thread_id) DO UPDATE SET updated_at = NOW(),
			user_id = CASE WHEN EXCLUDED.user_id <> '' THEN EXCLUDED.user_id ELSE conversations.user_id END
	`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, key.UserID)
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

// LastSkills is which capabilities the latest message was routed to, and when (domain.FollowUps): the person's latest
// message to this minion in any of their chats, so "and the next one?" on Telegram follows a question about email asked
// on the web a moment before (2026-10-31: one conversation per person and minion). Without a person, this chat's.
func (s *Store) LastSkills(ctx context.Context, key domain.ConversationKey) ([]string, time.Time) {
	var skills []string
	var at *time.Time
	var err error
	if key.UserID != "" {
		err = s.pool.QueryRow(ctx, `SELECT last_skills, last_skills_at FROM conversations
			WHERE agent_id = $1 AND user_id = $2 AND last_skills_at IS NOT NULL
			ORDER BY last_skills_at DESC LIMIT 1`, key.AgentID, key.UserID).Scan(&skills, &at)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT last_skills, last_skills_at FROM conversations
			WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4`,
			key.AgentID, key.IdentityID, key.ChatID, key.ThreadID).Scan(&skills, &at)
	}
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
		INSERT INTO conversations (agent_id, identity_id, chat_id, thread_id, updated_at, last_skills, last_skills_at, user_id)
		VALUES ($1, $2, $3, $4, NOW(), $5, NOW(), $6)
		ON CONFLICT (agent_id, identity_id, chat_id, thread_id) DO UPDATE SET last_skills = $5, last_skills_at = NOW(),
			user_id = CASE WHEN EXCLUDED.user_id <> '' THEN EXCLUDED.user_id ELSE conversations.user_id END
	`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, skills, key.UserID)
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
