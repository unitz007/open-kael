package postgres

import (
	"context"
	"encoding/json"
	"log"

	"github.com/unitz007/open-kael/domain"
)

const (
	memoryWindowSize      = 8
	memoryMaxContentBytes = 4000
	memoryContentOverflow = "[content too large — omitted]"
)

var _ domain.Memory = (*Store)(nil)

func (s *Store) History(ctx context.Context, key domain.ConversationKey) []domain.Message {
	rows, err := s.pool.Query(ctx, `
		SELECT role, content, tool_calls, tool_call_id, name
		FROM conversation_messages
		WHERE agent_id = $1 AND identity_id = $2 AND chat_id = $3 AND thread_id = $4
		ORDER BY id DESC
		LIMIT $5
	`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID, memoryWindowSize)
	if err != nil {
		log.Printf("memory: history agent=%s identity=%s chat=%s: %v", key.AgentID, key.IdentityID, key.ChatID, err)
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
		log.Printf("memory: rows agent=%s chat=%s: %v", key.AgentID, key.ChatID, err)
	}

	msgs := make([]domain.Message, len(reversed))
	for i, msg := range reversed {
		msgs[len(reversed)-1-i] = msg
	}
	return msgs
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
				(agent_id, identity_id, chat_id, thread_id, role, content, tool_calls, tool_call_id, name)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, key.AgentID, key.IdentityID, key.ChatID, key.ThreadID,
			string(msg.Role), content, toolJSON, msg.ToolCallID, msg.Name)
		if err != nil {
			log.Printf("memory: insert agent=%s chat=%s role=%s: %v", key.AgentID, key.ChatID, msg.Role, err)
		}
	}
}
