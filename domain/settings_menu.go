package domain

import "context"

// CallbackQueryAcknowledger is an optional interface executors may implement
// to dismiss the loading spinner on the user's client after a callback query
// (button tap) has been processed. The host calls it fire-and-forget after
// routing the action.
type CallbackQueryAcknowledger interface {
	AcknowledgeCallbackQuery(ctx context.Context, queryID string) error
}

// SettingsMenuProvider renders a navigable button menu in a messenger.
// Messengers implement this alongside InteractiveMessenger so the runtime
// host can deliver the settings flow through any supported channel without
// knowing which provider is behind it.
type SettingsMenuProvider interface {
	// PostSettingsMenu sends a new menu message and returns its opaque ID.
	PostSettingsMenu(ctx context.Context, channelRef string, menu *SettingsMenu) (messageID string, err error)
	// UpdateSettingsMenu edits an existing menu message in-place.
	UpdateSettingsMenu(ctx context.Context, channelRef, messageID string, menu *SettingsMenu) error
	// DeleteSettingsMenu removes the menu message from chat.
	DeleteSettingsMenu(ctx context.Context, channelRef, messageID string) error
}

// SettingsMenu is a titled list of rows rendered as a button menu.
// The provider translates it to the messenger's native format
// (Telegram InlineKeyboardMarkup, Slack Block Kit actions blocks, etc.).
type SettingsMenu struct {
	Title string
	Rows  []SettingsRow
}

// SettingsRow is one row in a SettingsMenu. It is either a section header
// (Header non-empty) or a row of one or more tappable buttons (Buttons set).
// Providers render headers as visually distinct labels — not as tappable items.
type SettingsRow struct {
	Buttons []SettingsButton
	Header  string // non-empty → section header label; Buttons is ignored
}

// SettingsButton is a single tappable button within a SettingsRow.
type SettingsButton struct {
	Label    string // display text, e.g. "✅ Make Transfer"
	Callback string // kael_sm:… callback data; empty for non-interactive labels
}

// Row builds a SettingsRow with the given buttons.
func Row(btns ...SettingsButton) SettingsRow { return SettingsRow{Buttons: btns} }

// Hdr builds a section-header SettingsRow. Providers render it as a visual
// label (bold text, a Block Kit header block, etc.) — never as a tappable button.
func Hdr(label string) SettingsRow { return SettingsRow{Header: label} }

// Btn builds a SettingsButton with the given label and callback.
func Btn(label, callback string) SettingsButton { return SettingsButton{Label: label, Callback: callback} }
