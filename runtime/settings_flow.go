package runtime

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/unitz007/open-kael/domain"
)

const settingsPageSize = 6

// menuSession tracks which screen and page a user is currently viewing so
// the flow can return to the right place after a toggle without encoding
// that state in the callback data (which would exceed Telegram's 64-byte
// callback_data limit when combined with a UUID tool ID).
type menuSession struct {
	integrationID string // non-empty when on the tool-list screen
	page          int
}

// SettingsFlow is the messenger-agnostic settings menu engine. It builds
// SettingsMenu values and delegates rendering to a SettingsMenuProvider.
// The sessions map is ephemeral — sessions are lost on restart, which means
// a stale menu in chat may navigate back to the integration list instead of
// the last-viewed page; this is acceptable for a settings flow.
type SettingsFlow struct {
	getApprovals func(ctx context.Context, userID string) (map[string]bool, error)
	setApproval  func(ctx context.Context, userID, toolID string, requires bool) error
	sessions     sync.Map // key: identityID+":"+channelRef → *menuSession
}

func newSettingsFlow(
	getApprovals func(ctx context.Context, userID string) (map[string]bool, error),
	setApproval func(ctx context.Context, userID, toolID string, requires bool) error,
) *SettingsFlow {
	return &SettingsFlow{getApprovals: getApprovals, setApproval: setApproval}
}

// Open posts the settings main menu for the user.
func (f *SettingsFlow) Open(ctx context.Context, provider domain.SettingsMenuProvider, hosted *HostedAgent, identityID, channelRef, userID string) {
	approvals, err := f.getApprovals(ctx, userID)
	if err != nil {
		log.Printf("runtime: settings: load approvals user %s: %v", userID, err)
		approvals = map[string]bool{}
	}
	sessionKey := identityID + ":" + channelRef
	f.sessions.Store(sessionKey, &menuSession{})
	menu := f.buildMainMenu(ctx, hosted, approvals)
	if _, err := provider.PostSettingsMenu(ctx, channelRef, menu); err != nil {
		log.Printf("runtime: settings: post menu channel %s: %v", channelRef, err)
	}
}

// Handle dispatches a kael_sm: callback action, mutates state if needed,
// and edits the existing menu message in-place.
func (f *SettingsFlow) Handle(ctx context.Context, provider domain.SettingsMenuProvider, hosted *HostedAgent, identityID, channelRef, messageID, userID, action string) {
	sessionKey := identityID + ":" + channelRef

	switch {
	case action == "close":
		f.sessions.Delete(sessionKey)
		if err := provider.DeleteSettingsMenu(ctx, channelRef, messageID); err != nil {
			log.Printf("runtime: settings: delete menu channel %s: %v", channelRef, err)
		}

	case action == "nav:main":
		f.sessions.Store(sessionKey, &menuSession{})
		approvals, _ := f.getApprovals(ctx, userID)
		menu := f.buildMainMenu(ctx, hosted, approvals)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case strings.HasPrefix(action, "nav:tools:"):
		// nav:tools:<integrationID>:<page>
		parts := strings.SplitN(action[len("nav:tools:"):], ":", 2)
		if len(parts) != 2 {
			return
		}
		integrationID := parts[0]
		page, _ := strconv.Atoi(parts[1])
		f.sessions.Store(sessionKey, &menuSession{integrationID: integrationID, page: page})
		approvals, _ := f.getApprovals(ctx, userID)
		menu := f.buildToolListMenu(hosted, integrationID, page, approvals)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case strings.HasPrefix(action, "toggle_approval:"):
		// toggle_approval:<toolID>:1|0
		rest := action[len("toggle_approval:"):]
		lastColon := strings.LastIndex(rest, ":")
		if lastColon < 0 {
			return
		}
		toolID := rest[:lastColon]
		requires := rest[lastColon+1:] == "1"
		if err := f.setApproval(ctx, userID, toolID, requires); err != nil {
			log.Printf("runtime: settings: set approval user %s tool %s: %v", userID, toolID, err)
			return
		}
		// Reload approvals and re-render the current screen.
		approvals, _ := f.getApprovals(ctx, userID)
		sess := &menuSession{}
		if v, ok := f.sessions.Load(sessionKey); ok {
			sess = v.(*menuSession)
		}
		var menu *domain.SettingsMenu
		if sess.integrationID != "" {
			menu = f.buildToolListMenu(hosted, sess.integrationID, sess.page, approvals)
		} else {
			menu = f.buildMainMenu(ctx, hosted, approvals)
		}
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)
	}
}

// buildMainMenu constructs the top-level settings screen.
// It lists each integration that the user has connected and that has at least
// one opt-in-able (non-mandatory) tool. Bot-level messenger integrations
// (Telegram, Slack, etc.) are excluded since they are not user-configurable.
func (f *SettingsFlow) buildMainMenu(ctx context.Context, hosted *HostedAgent, approvals map[string]bool) *domain.SettingsMenu {
	rows := []domain.SettingsRow{}

	// Bot-level integrations (the messenger channels themselves) are not
	// user-configurable — skip their tools.
	botIntegrationIDs := map[string]bool{}
	for _, identityID := range hosted.Agent.IdentityIDs {
		if identity, ok := hosted.Deps.IdentitiesByID[identityID]; ok {
			botIntegrationIDs[identity.IntegrationID] = true
		}
	}

	// Build a map of integrationID → identityID so we can check connection refs.
	integrationToIdentity := map[string]string{}
	for _, identity := range hosted.Deps.IdentitiesByID {
		integrationToIdentity[identity.IntegrationID] = identity.ID
	}

	// Collect integrations that have at least one non-mandatory tool AND that
	// the current user has actually connected (non-empty connection ref).
	type integrationEntry struct {
		id   string
		name string
	}
	var integrations []integrationEntry
	seen := map[string]bool{}
	for _, tool := range hosted.Deps.ToolsByID {
		if tool.RequiresApproval || botIntegrationIDs[tool.IntegrationID] {
			continue
		}
		if seen[tool.IntegrationID] {
			continue
		}
		// Only show integrations the user has connected.
		identityID := integrationToIdentity[tool.IntegrationID]
		if ref, _ := domain.ConnectionRefFromContext(ctx, identityID); ref == "" {
			continue
		}
		seen[tool.IntegrationID] = true
		name := tool.IntegrationID
		if intg, ok := hosted.Deps.IntegrationsByID[tool.IntegrationID]; ok {
			name = intg.Name
		}
		integrations = append(integrations, integrationEntry{id: tool.IntegrationID, name: name})
	}
	sort.Slice(integrations, func(i, j int) bool { return integrations[i].name < integrations[j].name })

	for _, intg := range integrations {
		rows = append(rows, domain.SettingsRow{
			Label:    intg.name + " →",
			Callback: "kael_sm:nav:tools:" + intg.id + ":0",
		})
	}
	rows = append(rows, domain.SettingsRow{Label: "📝 Personal Instructions", Callback: "kael_sm:instructions"})
	rows = append(rows, domain.SettingsRow{Label: "Close", Callback: "kael_sm:close"})

	title := "⚙️ Agent Settings\n\n" +
		"Here you can control how your agent behaves:\n\n" +
		"• Tap an integration to choose which tools ask for your approval before they run.\n" +
		"• Set Personal Instructions to tell the agent about yourself."
	return &domain.SettingsMenu{Title: title, Rows: rows}
}

// buildToolListMenu constructs the paginated tool-approval screen for one integration.
func (f *SettingsFlow) buildToolListMenu(hosted *HostedAgent, integrationID string, page int, approvals map[string]bool) *domain.SettingsMenu {
	// Collect opt-in-able tools for this integration, sorted by display name.
	var tools []*domain.ToolDefinition
	for _, t := range hosted.Deps.ToolsByID {
		if t.IntegrationID == integrationID && !t.RequiresApproval {
			tools = append(tools, t)
		}
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	totalPages := (len(tools) + settingsPageSize - 1) / settingsPageSize
	if page < 0 {
		page = 0
	}
	if page >= totalPages && totalPages > 0 {
		page = totalPages - 1
	}

	start := page * settingsPageSize
	end := start + settingsPageSize
	if end > len(tools) {
		end = len(tools)
	}
	pageTools := tools[start:end]

	integrationName := integrationID
	if intg, ok := hosted.Deps.IntegrationsByID[integrationID]; ok {
		integrationName = intg.Name
	}

	title := "⚙️ " + integrationName
	if totalPages > 1 {
		title = fmt.Sprintf("%s  (%d/%d)", title, page+1, totalPages)
	}
	title += "\n\nTap a tool to toggle whether the agent asks for your approval before using it. ✅ means approval required."

	rows := make([]domain.SettingsRow, 0, len(pageTools)+3)
	for _, t := range pageTools {
		label := "◻  " + t.Name
		newVal := "1"
		if approvals[t.ID] {
			label = "✅ " + t.Name
			newVal = "0"
		}
		rows = append(rows, domain.SettingsRow{
			Label:    label,
			Callback: "kael_sm:toggle_approval:" + t.ID + ":" + newVal,
		})
	}

	// Pagination row
	if page > 0 {
		rows = append(rows, domain.SettingsRow{
			Label:    "← Prev",
			Callback: "kael_sm:nav:tools:" + integrationID + ":" + strconv.Itoa(page-1),
		})
	}
	if page < totalPages-1 {
		rows = append(rows, domain.SettingsRow{
			Label:    "Next →",
			Callback: "kael_sm:nav:tools:" + integrationID + ":" + strconv.Itoa(page+1),
		})
	}
	rows = append(rows, domain.SettingsRow{Label: "← Back", Callback: "kael_sm:nav:main"})

	return &domain.SettingsMenu{Title: title, Rows: rows}
}
