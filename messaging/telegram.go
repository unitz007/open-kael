package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

const telegramPlatform = "telegram"

type TelegramBot struct {
	Token  string
	ChatId string
}

// NewTelegramBot builds a bot identity from the given env var names, so
// multiple agents can each have their own Telegram bot token (tokenEnv)
// while still addressing the same human (chatIdEnv is typically shared
// across agents — a Telegram user's chat ID is the same regardless of
// which bot is messaging them).
func NewTelegramBot(tokenEnv, chatIdEnv string) *TelegramBot {

	telegramToken, ok := os.LookupEnv(tokenEnv)
	if !ok {
		slog.Debug(tokenEnv + " is not set")
	}

	telegramChatId, ok := os.LookupEnv(chatIdEnv)
	if !ok {
		slog.Debug(chatIdEnv + " is not set")

	}

	return &TelegramBot{
		Token:  telegramToken,
		ChatId: telegramChatId,
	}
}

func (b *TelegramBot) Platform() string {
	return telegramPlatform
}

func (b *TelegramBot) DefaultConversation() ConversationRef {
	return ConversationRef{Platform: telegramPlatform, ChatID: b.ChatId}
}

func (b *TelegramBot) Send(ctx context.Context, conv ConversationRef, message string) error {

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", b.Token)

	message = formatForTelegram(message)

	data := url.Values{}
	data.Set("chat_id", conv.ChatID)
	data.Set("text", message)
	data.Set("parse_mode", "HTML")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			slog.Error("Error closing response body", "error", err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API returned status %d: %s", resp.StatusCode, string(body))
	}
	log.Printf("📤telegram: sent message to chat %s", conv.ChatID)
	return nil
}

// telegramUpdatesResponse is the subset of Telegram's getUpdates response
// this cares about — just enough to route a text message, plus the error
// fields Telegram sends when Ok is false (e.g. a 409 Conflict from a second
// process polling the same bot token, or a bad token). Those decode into a
// perfectly valid, empty-Result response otherwise — indistinguishable from
// "no new messages right now" unless Ok/Description are actually checked.
type telegramUpdatesResponse struct {
	Ok          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Result      []struct {
		UpdateID int `json:"update_id"`
		Message  *struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			Text string `json:"text"`
		} `json:"message"`
	} `json:"result"`
}

// Listen long-polls Telegram's getUpdates endpoint — no webhook server
// needed, just an outbound connection, which fits this project's existing
// "no infra beyond outbound HTTP" style. Stops cleanly when ctx is
// cancelled.
func (b *TelegramBot) Listen(ctx context.Context, onMessage func(InboundMessage)) error {
	offset := 0
	client := &http.Client{Timeout: 40 * time.Second}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", b.Token, offset)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Println("telegram getUpdates error:", err)
			time.Sleep(2 * time.Second)
			continue
		}

		var body telegramUpdatesResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if decodeErr != nil {
			log.Println("telegram getUpdates decode error:", decodeErr)
			time.Sleep(2 * time.Second)
			continue
		}

		if !body.Ok {
			// A failure here (bad token, or a 409 "Conflict: terminated by
			// other getUpdates request" from a second process polling the
			// same bot) still decodes as valid JSON with an empty Result —
			// silently indistinguishable from "no new messages" unless
			// logged explicitly here.
			log.Printf("telegram getUpdates failed (code %d): %s", body.ErrorCode, body.Description)
			time.Sleep(2 * time.Second)
			continue
		}

		for _, update := range body.Result {
			offset = update.UpdateID + 1
			if update.Message == nil || update.Message.Text == "" {
				continue
			}
			chatID := strconv.FormatInt(update.Message.Chat.ID, 10)
			log.Printf("📥telegram: received message from chat %s: %q", chatID, update.Message.Text)
			onMessage(InboundMessage{
				Conversation: ConversationRef{
					Platform: telegramPlatform,
					ChatID:   chatID,
				},
				Text: update.Message.Text,
			})
		}
	}
}

// rejoinOrphanedBullets fixes a malformed-list pattern the model occasionally
// produces — a bullet marker alone on its own line with the item's actual
// text on the next line — by joining them before the markdown parser sees
// it, since a lone "-" isn't recognized as a valid list item and would
// otherwise survive into the output as a stray dash.
func rejoinOrphanedBullets(s string) string {
	return regexp.MustCompile(`(?m)^([-•])\s*\n+`).ReplaceAllString(s, "$1 ")
}

var (
	orderedListBlockRe = regexp.MustCompile(`(?s)<ol>(.*?)</ol>`)
	// \s* swallows the whitespace goldmark inserts between "<li>" and a
	// following "<p>" for a "loose" list (items separated by a blank line
	// in the model's markdown) — without it, "1. " and the item's text end
	// up on separate lines once <p> is stripped further down the pipeline.
	listItemOpenTagRe = regexp.MustCompile(`<li>\s*`)
)

// numberOrderedLists replaces "<li>" with a sequential "N. " inside each
// <ol>...</ol> block (and drops the now-redundant wrapper tags), so a
// numbered list keeps its order instead of flattening into bullets like
// every other <li> does. "</li>" is left alone — the later generic pass
// still converts it to a newline for both list kinds.
//
// This only handles one level of nesting correctly: a non-greedy regex
// can't tell an outer <ol> from an <ol> nested inside one of its own <li>
// items. LLM output is overwhelmingly flat single-level lists, and the rest
// of this file's tag handling is regex-based too, so a full HTML parser
// felt like more machinery than the actual problem warranted.
func numberOrderedLists(html string) string {
	return orderedListBlockRe.ReplaceAllStringFunc(html, func(block string) string {
		inner := block[len("<ol>") : len(block)-len("</ol>")]
		n := 0
		return listItemOpenTagRe.ReplaceAllStringFunc(inner, func(string) string {
			n++
			return fmt.Sprintf("%d. ", n)
		})
	})
}

func formatForTelegram(message string) string {
	message = rejoinOrphanedBullets(message)

	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(message), &buf); err != nil {
		return message
	}

	html := buf.String()

	for _, tag := range []string{"h1", "h2", "h3", "h4"} {
		html = strings.ReplaceAll(html, "<"+tag+">", "<b>")
		html = strings.ReplaceAll(html, "</"+tag+">", "</b>\n")
	}

	html = numberOrderedLists(html) // <ol>/<li> -> "1. " before <li> is flattened to a bullet below

	html = strings.ReplaceAll(html, "<ul>", "")
	html = strings.ReplaceAll(html, "</ul>", "\n")
	html = listItemOpenTagRe.ReplaceAllString(html, "• ") // whatever <li> remains belongs to an unordered list
	html = strings.ReplaceAll(html, "</li>", "\n")

	html = strings.ReplaceAll(html, "<p>", "")
	html = strings.ReplaceAll(html, "</p>", "\n") // single newline, not double

	// Telegram's HTML parse_mode has no <hr> — goldmark renders a thematic
	// break (---) as one regardless, and Telegram rejects the whole message
	// outright on an unrecognized tag rather than stripping it, so swap it
	// for a plain-text divider instead of letting the send fail.
	html = strings.ReplaceAll(html, "<hr />", "———")
	html = strings.ReplaceAll(html, "<hr/>", "———")
	html = strings.ReplaceAll(html, "<hr>", "———")

	// collapse any run of 2+ newlines (from stacked tag replacements) down to exactly one
	html = regexp.MustCompile(`\n{2,}`).ReplaceAllString(html, "\n")

	return strings.TrimSpace(html)
}
