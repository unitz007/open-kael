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

// SettingsRow is one button row in a SettingsMenu.
// A row with an empty Callback is rendered as a non-interactive label or
// visual separator, at the provider's discretion.
type SettingsRow struct {
	Label    string // display text shown on the button, e.g. "✅ Make Transfer"
	Callback string // kael_sm:… callback data; empty for separators
}
