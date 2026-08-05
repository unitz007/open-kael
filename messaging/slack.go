package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yuin/goldmark"
)

const slackPlatform = "slack"

// slackHTTPClient is used for every Slack Web API call (chat.postMessage,
// apps.connections.open) — separate from the long-lived Socket Mode
// WebSocket connection itself, which has its own lifecycle. Explicit
// timeout so a stalled request surfaces as a real error instead of
// blocking indefinitely (http.DefaultClient has none).
var slackHTTPClient = &http.Client{Timeout: 30 * time.Second}

// SlackBot implements Messenger over Slack's Web API (outbound) and Socket
// Mode (inbound). Socket Mode is Slack's equivalent of Telegram's
// long-polling: an outbound WebSocket connection this process opens to
// Slack, rather than a public endpoint Slack would need to reach — same
// "no infra beyond outbound connections" shape as telegram.go.
type SlackBot struct {
	BotToken  string // xoxb-... — Web API calls (chat.postMessage)
	AppToken  string // xapp-... — opening the Socket Mode connection
	ChannelId string // default channel for a proactive send with no active conversation

	// botUserID is this bot's own Slack user ID (e.g. "U0BMYK0B3M2"),
	// resolved once via auth.test at the start of Listen. It's how
	// listenOnce tells "someone @-mentioned me" apart from "someone
	// @-mentioned a different app that also happens to be in this
	// channel" — Slack delivers the same channel message event to every
	// app that's a member, regardless of which one was actually mentioned.
	botUserID string

	// RespondWhenUnmentioned makes this bot also treat a channel message as
	// its own when the message mentions no bot at all — the "default
	// assistant" behavior, so a plain unaddressed message still gets a
	// reply instead of silently going nowhere. Only meant for one bot per
	// workspace (Personal Assistant's "Kael AI"); leave false (the zero
	// value) for every other identity, which should only ever respond to
	// its own explicit mention.
	RespondWhenUnmentioned bool

	// SiblingBotUserIDs lists the other bots' Slack user IDs in the same
	// workspace, so RespondWhenUnmentioned can tell "nobody was mentioned"
	// apart from "a different bot was mentioned" — without this, a message
	// meant for e.g. Kael Dev would also wrongly trigger the default
	// assistant's fallback. Only meaningful when RespondWhenUnmentioned is
	// true.
	SiblingBotUserIDs []string
}

// NewSlackBot builds a bot identity from the given env var names, so
// multiple agents can each have their own Slack app (distinct bot/app
// tokens) while still defaulting to the same channel (channelIdEnv is
// typically shared across agents).
func NewSlackBot(botTokenEnv, appTokenEnv, channelIdEnv string) *SlackBot {
	botToken, ok := os.LookupEnv(botTokenEnv)
	if !ok {
		slog.Debug(botTokenEnv + " is not set")
	}

	appToken, ok := os.LookupEnv(appTokenEnv)
	if !ok {
		slog.Debug(appTokenEnv + " is not set")
	}

	channelId, ok := os.LookupEnv(channelIdEnv)
	if !ok {
		slog.Debug(channelIdEnv + " is not set")
	}

	return &SlackBot{
		BotToken:  botToken,
		AppToken:  appToken,
		ChannelId: channelId,
	}
}

func (b *SlackBot) Platform() string {
	return slackPlatform
}

func (b *SlackBot) DefaultConversation() ConversationRef {
	return ConversationRef{Platform: slackPlatform, ChatID: b.ChannelId}
}

type slackAPIResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
}

func (b *SlackBot) Send(ctx context.Context, conv ConversationRef, message string) error {
	payload := map[string]string{
		"channel": conv.ChatID,
		"text":    formatForSlack(message),
	}
	// Reply into the same thread the triggering message came from, if any —
	// a proactive/cron-triggered send has no thread in ctx and just posts a
	// new top-level message, same as before this existed.
	if threadID, ok := ThreadIDFromContext(ctx); ok {
		payload["thread_ts"] = threadID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+b.BotToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result slackAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Ok {
		return fmt.Errorf("slack chat.postMessage failed: %s", result.Error)
	}

	log.Printf("📤slack: sent message to channel %s", conv.ChatID)
	return nil
}

// AddReaction adds an emoji reaction (name without colons, e.g.
// "thumbsup") to a specific message — channel + that message's own ts, not
// a ConversationRef, since a conversation has no single timestamp of its
// own.
func (b *SlackBot) AddReaction(ctx context.Context, channel, timestamp, emoji string) error {
	body, err := json.Marshal(map[string]string{
		"channel":   channel,
		"timestamp": timestamp,
		"name":      emoji,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/reactions.add", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+b.BotToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result slackAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.Ok {
		return fmt.Errorf("slack reactions.add failed: %s", result.Error)
	}

	log.Printf("📤slack: added reaction :%s: in channel %s", emoji, channel)
	return nil
}

// ListCustomEmoji returns this workspace's custom emoji, name -> either an
// image URL or, for an alias, "alias:<other-name>". Slack's emoji.list only
// covers custom emoji, not the standard set (thumbsup, tada, ...) — those
// are common knowledge an LLM already has; this exists specifically to
// surface the workspace-specific names it has no way to already know.
func (b *SlackBot) ListCustomEmoji(ctx context.Context) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/emoji.list", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.BotToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		slackAPIResponse
		Emoji map[string]string `json:"emoji"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.Ok {
		return nil, fmt.Errorf("slack emoji.list failed: %s", result.Error)
	}
	return result.Emoji, nil
}

// slackConnectionsOpenResponse is apps.connections.open's response — just
// enough to dial the WebSocket it hands back.
type slackConnectionsOpenResponse struct {
	slackAPIResponse
	Url string `json:"url"`
}

// slackAuthTestResponse is auth.test's response — just enough to learn this
// bot token's own user ID.
type slackAuthTestResponse struct {
	slackAPIResponse
	UserId string `json:"user_id"`
}

// authTest resolves this bot's own Slack user ID, so listenOnce can tell
// "I was mentioned" apart from "some other app in this channel was
// mentioned."
func (b *SlackBot) authTest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/auth.test", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.BotToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result slackAuthTestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Ok {
		return "", fmt.Errorf("slack auth.test failed: %s", result.Error)
	}
	return result.UserId, nil
}

// openSocketModeURL asks Slack for a fresh Socket Mode WebSocket URL.
// Each URL is single-use and short-lived, so Listen requests a new one
// every time it (re)connects rather than caching one.
func (b *SlackBot) openSocketModeURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/apps.connections.open", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.AppToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result slackConnectionsOpenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.Ok {
		return "", fmt.Errorf("slack apps.connections.open failed: %s", result.Error)
	}
	return result.Url, nil
}

// slackEnvelope is the outer shape of every Socket Mode message. Payload's
// actual structure depends on Type, so it's decoded again separately once
// Type is known.
type slackEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeId string          `json:"envelope_id"`
	Reason     string          `json:"reason"`
	Payload    json.RawMessage `json:"payload"`
}

type slackEventPayload struct {
	Event struct {
		Type        string `json:"type"`
		Subtype     string `json:"subtype"`
		Channel     string `json:"channel"`
		ChannelType string `json:"channel_type"` // "channel", "group", "im", "mpim"
		Text        string `json:"text"`
		BotId       string `json:"bot_id"`
		Ts          string `json:"ts"`        // this message's own id — what reactions.add needs as "timestamp"
		ThreadTs    string `json:"thread_ts"` // set only when this message is itself a reply within an existing thread
	} `json:"event"`
}

// Listen opens a Socket Mode connection and reconnects for as long as ctx
// stays alive. Reconnection isn't just error recovery — Slack periodically
// sends a "disconnect" envelope on its own for cluster maintenance and
// expects the client to open a fresh connection in response, same as a
// dropped connection.
func (b *SlackBot) Listen(ctx context.Context, onMessage func(InboundMessage)) error {
	if b.botUserID == "" {
		userID, err := b.authTest(ctx)
		if err != nil {
			return fmt.Errorf("slack: could not resolve own user ID via auth.test: %w", err)
		}
		b.botUserID = userID
		log.Printf("slack: listening as %s", b.botUserID)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if err := b.listenOnce(ctx, onMessage); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Println("slack socket mode error:", err)
			time.Sleep(2 * time.Second)
		}
	}
}

// listenOnce runs a single Socket Mode connection until it's told to
// reconnect, drops, or ctx is cancelled.
func (b *SlackBot) listenOnce(ctx context.Context, onMessage func(InboundMessage)) error {
	wsUrl, err := b.openSocketModeURL(ctx)
	if err != nil {
		return err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsUrl, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	// conn.ReadMessage below has no context awareness of its own — closing
	// the connection out from under it is what actually makes it return
	// promptly once ctx is cancelled, instead of blocking until Slack's
	// own connection lifetime ends.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		var env slackEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			log.Println("slack: malformed socket mode envelope:", err)
			continue
		}

		switch env.Type {
		case "disconnect":
			log.Printf("slack: reconnect requested (%s)", env.Reason)
			return nil

		case "events_api":
			// Ack immediately, regardless of how onMessage below turns out —
			// Slack resends the event if an ack doesn't arrive within ~3s.
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeId})
			if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
				log.Println("slack: failed to ack event:", err)
			}

			var payload slackEventPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				log.Println("slack: malformed event payload:", err)
				continue
			}

			event := payload.Event
			if event.Type != "message" || event.Text == "" || event.BotId != "" || event.Subtype != "" {
				// Not a plain human text message — in particular BotId set
				// (including this bot's own messages) and Subtype set
				// (edits, channel-join notices, etc.) are skipped, so the
				// bot never replies to itself in a loop.
				continue
			}

			mention := "<@" + b.botUserID + ">"
			mentioned := strings.Contains(event.Text, mention)

			// RespondWhenUnmentioned's fallback only applies to a channel
			// message that mentions no bot at all — one that mentions a
			// sibling bot is that bot's to handle, not a "nobody claimed
			// this" case, or a message meant for Kael Dev would also wrongly
			// reach the default assistant.
			isFallback := !mentioned && event.ChannelType != "im" && b.RespondWhenUnmentioned
			if isFallback {
				for _, sib := range b.SiblingBotUserIDs {
					if sib != "" && strings.Contains(event.Text, "<@"+sib+">") {
						isFallback = false
						break
					}
				}
			}

			// In channels/groups, Slack delivers this same event to every
			// app that's a member, not just the one @-mentioned — so a
			// channel message only counts as "for this bot" if it actually
			// mentions this bot, or falls through to it via
			// RespondWhenUnmentioned. DMs have no other participant it could
			// be meant for, so those always count.
			if event.ChannelType != "im" && !mentioned && !isFallback {
				continue
			}
			text := event.Text
			if mentioned {
				text = strings.TrimSpace(strings.ReplaceAll(text, mention, ""))
			}

			log.Printf("📥slack: received message from channel %s: %q", event.Channel, text)
			// The thread a reply should land in: the existing thread's root
			// if this message is already inside one, otherwise this
			// message's own ts (replying to a top-level message starts a
			// new thread anchored to it).
			threadID := event.ThreadTs
			if threadID == "" {
				threadID = event.Ts
			}

			onMessage(InboundMessage{
				Conversation: ConversationRef{Platform: slackPlatform, ChatID: event.Channel},
				Text:         text,
				MessageID:    event.Ts,
				ThreadID:     threadID,
			})

		default:
			// "hello" (initial handshake) and anything else — nothing to do.
		}
	}
}

// slackLinkRe extracts goldmark's rendered <a href="...">text</a> so it can
// be rewritten as Slack's own "<url|text>" link syntax.
var slackLinkRe = regexp.MustCompile(`<a href="([^"]*)">(.*?)</a>`)

// formatForSlack converts the model's markdown output into Slack's mrkdwn
// syntax — a different, non-standard flavor of markdown (single asterisks
// for bold rather than double, underscores for italic, "<url|text>" links
// instead of "[text](url)", no header syntax at all) that a bare markdown
// string sent as-is to chat.postMessage renders completely unformatted —
// literal asterisks, brackets, and "#"s included.
//
// Same two-stage pipeline as telegram.go's formatForTelegram, and reuses
// its list-numbering/bullet-flattening helpers directly (they operate on
// generic <ol>/<li> HTML, nothing Telegram-specific): parse real markdown
// into HTML via goldmark first, since regexing raw markdown asterisks
// directly is ambiguous (bold vs italic vs list marker depends on
// context) — then rewrite that unambiguous HTML into Slack's syntax.
func formatForSlack(message string) string {
	message = rejoinOrphanedBullets(message)

	var buf bytes.Buffer
	if err := goldmark.Convert([]byte(message), &buf); err != nil {
		return message
	}
	html := buf.String()

	// No header syntax in Slack mrkdwn — bold reads as the closest
	// equivalent for a section title.
	for _, tag := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		html = strings.ReplaceAll(html, "<"+tag+">", "*")
		html = strings.ReplaceAll(html, "</"+tag+">", "*\n")
	}

	html = strings.ReplaceAll(html, "<strong>", "*")
	html = strings.ReplaceAll(html, "</strong>", "*")
	html = strings.ReplaceAll(html, "<em>", "_")
	html = strings.ReplaceAll(html, "</em>", "_")

	html = slackLinkRe.ReplaceAllString(html, "<$1|$2>")

	// Code blocks before inline code — a code block is "<pre><code>...",
	// and handling the pair first means the leftover bare <code>/</code>
	// pass below only ever matches genuinely inline code.
	html = strings.ReplaceAll(html, "<pre><code>", "```")
	html = strings.ReplaceAll(html, "</code></pre>", "```\n")
	html = strings.ReplaceAll(html, "<code>", "`")
	html = strings.ReplaceAll(html, "</code>", "`")

	html = numberOrderedLists(html) // shared with Telegram — <ol>/<li> -> "1. " before <li> is flattened below

	html = strings.ReplaceAll(html, "<ul>", "")
	html = strings.ReplaceAll(html, "</ul>", "\n")
	html = listItemOpenTagRe.ReplaceAllString(html, "• ") // whatever <li> remains belongs to an unordered list
	html = strings.ReplaceAll(html, "</li>", "\n")

	// Blockquotes render as plain text here, not Slack's "> " quote
	// styling — LLM output rarely uses them, so the extra per-line-prefix
	// logic that'd take didn't seem worth it for a first pass.
	html = strings.ReplaceAll(html, "<blockquote>", "")
	html = strings.ReplaceAll(html, "</blockquote>", "\n")

	html = strings.ReplaceAll(html, "<p>", "")
	html = strings.ReplaceAll(html, "</p>", "\n")

	// Slack's plain-text field has no <hr> equivalent — same plain-text
	// divider swap formatForTelegram uses.
	html = strings.ReplaceAll(html, "<hr />", "———")
	html = strings.ReplaceAll(html, "<hr/>", "———")
	html = strings.ReplaceAll(html, "<hr>", "———")

	html = regexp.MustCompile(`\n{2,}`).ReplaceAllString(html, "\n")

	// Slack's mrkdwn requires &, <, and > to stay escaped — < and > are
	// its own link/mention syntax — and goldmark already escaped them
	// correctly for that purpose, so only quotes/apostrophes need
	// unescaping back; those aren't special in Slack's syntax.
	html = strings.ReplaceAll(html, "&quot;", "\"")
	html = strings.ReplaceAll(html, "&#39;", "'")

	return strings.TrimSpace(html)
}
