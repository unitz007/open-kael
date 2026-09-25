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

func (s *Store) History(ctx context.Context, id string) []domain.Message {
	rows, err := s.pool.Query(ctx, `
		SELECT role, content, tool_calls, tool_call_id, name
		FROM conversation_messages
		WHERE conv_id = $1
		ORDER BY id DESC
		LIMIT $2
	`, id, memoryWindowSize)
	if err != nil {
		log.Printf("memory: history %q: %v", id, err)
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
			log.Printf("memory: scan %q: %v", id, err)
			continue
		}
		var calls []domain.ToolCall
		if len(toolJSON) > 2 { // more than just "[]"
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
		log.Printf("memory: rows %q: %v", id, err)
	}

	msgs := make([]domain.Message, len(reversed))
	for i, msg := range reversed {
		msgs[len(reversed)-1-i] = msg
	}
	return msgs
}

func (s *Store) Append(ctx context.Context, id string, messages ...domain.Message) {
	if len(messages) == 0 {
		return
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (id, updated_at)
		VALUES ($1, NOW())
		ON CONFLICT (id) DO UPDATE SET updated_at = NOW()
	`, id)
	if err != nil {
		log.Printf("memory: upsert conversation %q: %v", id, err)
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
			INSERT INTO conversation_messages (conv_id, role, content, tool_calls, tool_call_id, name)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, id, string(msg.Role), content, toolJSON, msg.ToolCallID, msg.Name)
		if err != nil {
			log.Printf("memory: insert %q role=%s: %v", id, msg.Role, err)
		}
	}
}
