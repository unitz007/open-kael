package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/unitz007/kael/messaging"
	"github.com/unitz007/kael/tools"
)

// Tools implements ToolProvider — add_reaction and search_emoji, both
// Slack-specific (no equivalent concept on other platforms), so they ship
// as SlackBot's own contribution rather than living in the generic
// Messenger interface. Any agent with a SlackBot registered picks these up
// automatically via Agent.baseTools().
func (b *SlackBot) Tools() []*tools.ToolSpec {
	return []*tools.ToolSpec{b.addReactionTool(), b.searchEmojiTool()}
}

// addReactionTool reacts to whichever inbound Slack message triggered the
// current run — closes over b directly rather than resolving it from ctx,
// since b already exists at construction time.
func (b *SlackBot) addReactionTool() *tools.ToolSpec {
	return tools.NewToolBuilder("add_reaction", "Adds an emoji reaction to the Slack message you're currently responding to — use for a quick acknowledgement instead of, or in addition to, a text reply.").
		Parameter("emoji", "string", "The reaction name, without colons (e.g. \"thumbsup\", \"eyes\", \"white_check_mark\")", true).
		Platform("slack").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			var input struct {
				Emoji string `json:"emoji"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			conv, ok := messaging.ConversationFromContext(ctx)
			if !ok {
				return nil, fmt.Errorf("add_reaction: no active conversation to react in")
			}
			messageID, ok := messaging.MessageIDFromContext(ctx)
			if !ok {
				return nil, fmt.Errorf("add_reaction: no specific message to react to (only works when replying to an inbound Slack message)")
			}

			if err := b.AddReaction(ctx, conv.ChatID, messageID, input.Emoji); err != nil {
				return nil, err
			}
			return fmt.Sprintf("Added :%s: reaction.", input.Emoji), nil
		}).Build()
}

// searchEmojiTool looks up this Slack workspace's own custom emoji — the
// standard set (thumbsup, tada, ...) needs no lookup, an LLM already knows
// those names, but a custom emoji this specific workspace added is
// otherwise unguessable. Use before add_reaction (or send_message, for an
// inline :name:) whenever a workspace-specific emoji might fit better than
// a standard one, or to confirm a name actually exists before using it.
func (b *SlackBot) searchEmojiTool() *tools.ToolSpec {
	return tools.NewToolBuilder("search_emoji", "Searches this Slack workspace's custom emoji (not the standard set, which needs no lookup) by substring match on name. Empty query lists all of them.").
		Parameter("query", "string", "Substring to match against emoji names (case-insensitive, can be empty)", false).
		Platform("slack").
		Handler(func(ctx context.Context, args json.RawMessage) (any, error) {
			var raw string
			if err := json.Unmarshal(args, &raw); err != nil {
				return nil, err
			}
			var input struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				return nil, err
			}

			emoji, err := b.ListCustomEmoji(ctx)
			if err != nil {
				return nil, err
			}

			query := strings.ToLower(input.Query)
			names := make([]string, 0, len(emoji))
			for name := range emoji {
				if query == "" || strings.Contains(strings.ToLower(name), query) {
					names = append(names, name)
				}
			}
			sort.Strings(names)

			if len(names) == 0 {
				return "No matching custom emoji found.", nil
			}
			var sb strings.Builder
			for _, name := range names {
				value := emoji[name]
				if strings.HasPrefix(value, "alias:") {
					fmt.Fprintf(&sb, "- :%s: (alias for :%s:)\n", name, strings.TrimPrefix(value, "alias:"))
				} else {
					fmt.Fprintf(&sb, "- :%s:\n", name)
				}
			}
			return sb.String(), nil
		}).Build()
}
