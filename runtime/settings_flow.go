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
	getApprovals        func(ctx context.Context, userID string) (map[string]bool, error)
	setApproval         func(ctx context.Context, userID, toolID string, requires bool) error
	checkLinkedEmail    func(ctx context.Context, userID string) (bool, error)
	getAppInstructions  func(ctx context.Context, userID, integrationID string) (string, error)
	setAppInstructions  func(ctx context.Context, userID, integrationID, text string) error
	sessions            sync.Map // key: identityID+":"+channelRef → *menuSession
}

func newSettingsFlow(
	getApprovals func(ctx context.Context, userID string) (map[string]bool, error),
	setApproval func(ctx context.Context, userID, toolID string, requires bool) error,
) *SettingsFlow {
	return &SettingsFlow{getApprovals: getApprovals, setApproval: setApproval}
}

func (f *SettingsFlow) isEmailLinked(ctx context.Context, userID string) bool {
	if f.checkLinkedEmail == nil || userID == "" {
		return true // treat as linked when checker is absent so the button stays hidden
	}
	linked, _ := f.checkLinkedEmail(ctx, userID)
	return linked
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
	menu := f.buildMainMenu(ctx, hosted, approvals, userID)
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

	case action == "noop":
		// Section header tapped — nothing to do; the spinner is dismissed by
		// the caller's AcknowledgeCallbackQuery after Handle returns.

	case action == "nav:main":
		f.sessions.Store(sessionKey, &menuSession{})
		approvals, _ := f.getApprovals(ctx, userID)
		menu := f.buildMainMenu(ctx, hosted, approvals, userID)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case action == "nav:instructions":
		f.sessions.Store(sessionKey, &menuSession{})
		menu := f.buildInstructionsMenu(hosted)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case action == "nav:account":
		f.sessions.Store(sessionKey, &menuSession{})
		menu := f.buildAccountMenu(ctx, userID)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case strings.HasPrefix(action, "nav:app:") && !strings.HasPrefix(action, "nav:app_instr:"):
		integrationID := action[len("nav:app:"):]
		f.sessions.Store(sessionKey, &menuSession{integrationID: integrationID})
		approvals, _ := f.getApprovals(ctx, userID)
		menu := f.buildAppMenu(hosted, integrationID, approvals)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case strings.HasPrefix(action, "nav:app_instr:"):
		integrationID := action[len("nav:app_instr:"):]
		f.sessions.Store(sessionKey, &menuSession{integrationID: integrationID})
		integrationName := integrationID
		if intg, ok := hosted.Deps.IntegrationsByID[integrationID]; ok {
			integrationName = intg.Name
		}
		menu := f.buildAppInstructionsMenu(ctx, userID, integrationID, integrationName)
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)

	case strings.HasPrefix(action, "app_instr_clear:"):
		integrationID := action[len("app_instr_clear:"):]
		if f.setAppInstructions != nil {
			_ = f.setAppInstructions(ctx, userID, integrationID, "")
		}
		integrationName := integrationID
		if intg, ok := hosted.Deps.IntegrationsByID[integrationID]; ok {
			integrationName = intg.Name
		}
		menu := f.buildAppInstructionsMenu(ctx, userID, integrationID, integrationName)
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
			menu = f.buildMainMenu(ctx, hosted, approvals, userID)
		}
		_ = provider.UpdateSettingsMenu(ctx, channelRef, messageID, menu)
	}
}

// buildMainMenu constructs the top-level settings screen.
func (f *SettingsFlow) buildMainMenu(ctx context.Context, hosted *HostedAgent, approvals map[string]bool, userID string) *domain.SettingsMenu {
	rows := []domain.SettingsRow{}

	// Bot-level integrations (the messenger channels themselves) are not
	// user-configurable — skip their tools.
	botIntegrationIDs := map[string]bool{}
	for _, identityID := range hosted.Agent.IdentityIDs {
		if identity, ok := hosted.Deps.IdentitiesByID[identityID]; ok {
			botIntegrationIDs[identity.IntegrationID] = true
		}
	}

	integrationToIdentity := map[string]string{}
	for _, identity := range hosted.Deps.IdentitiesByID {
		integrationToIdentity[identity.IntegrationID] = identity.ID
	}

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

	if len(integrations) > 0 {
		rows = append(rows, domain.Row(domain.Btn("── Apps ──", "kael_sm:noop")))
		for _, intg := range integrations {
			rows = append(rows, domain.Row(domain.Btn(intg.name+" →", "kael_sm:nav:app:"+intg.id)))
		}
	}

	rows = append(rows, domain.Row(domain.Btn("── Preferences ──", "kael_sm:noop")))

	linkLabel := "🔗 Link Account"
	if f.isEmailLinked(ctx, userID) {
		linkLabel = "✅ Account Linked"
	}
	rows = append(rows, domain.Row(
		domain.Btn("💬 Instructions", "kael_sm:nav:instructions"),
		domain.Btn(linkLabel, "kael_sm:nav:account"),
	))

	rows = append(rows, domain.Row(domain.Btn("✕  Close", "kael_sm:close")))

	return &domain.SettingsMenu{Title: "⚙️ " + hosted.Agent.Name, Rows: rows}
}

// buildToolListMenu constructs the paginated tool-approval screen for one
// integration. Tools are shown in a 2-column grid for compact display.
func (f *SettingsFlow) buildToolListMenu(hosted *HostedAgent, integrationID string, page int, approvals map[string]bool) *domain.SettingsMenu {
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
	title += "\nTap to toggle — ✅ asks for your confirm before the tool runs"

	rows := make([]domain.SettingsRow, 0, len(pageTools)/2+3)
	for i := 0; i < len(pageTools); i += 2 {
		b1 := toolButton(pageTools[i], approvals)
		if i+1 < len(pageTools) {
			rows = append(rows, domain.Row(b1, toolButton(pageTools[i+1], approvals)))
		} else {
			rows = append(rows, domain.Row(b1))
		}
	}

	if page > 0 {
		rows = append(rows, domain.Row(domain.Btn("← Prev", "kael_sm:nav:tools:"+integrationID+":"+strconv.Itoa(page-1))))
	}
	if page < totalPages-1 {
		rows = append(rows, domain.Row(domain.Btn("Next →", "kael_sm:nav:tools:"+integrationID+":"+strconv.Itoa(page+1))))
	}
	rows = append(rows, domain.Row(
		domain.Btn("← Back", "kael_sm:nav:app:"+integrationID),
		domain.Btn("✕  Close", "kael_sm:close"),
	))

	return &domain.SettingsMenu{Title: title, Rows: rows}
}

// toolButton builds the SettingsButton for one tool, showing its current
// approval state and toggling it on tap.
func toolButton(t *domain.ToolDefinition, approvals map[string]bool) domain.SettingsButton {
	if approvals[t.ID] {
		return domain.Btn("✅ "+t.Name, "kael_sm:toggle_approval:"+t.ID+":0")
	}
	return domain.Btn("☐  "+t.Name, "kael_sm:toggle_approval:"+t.ID+":1")
}

// buildInstructionsMenu constructs the personal-instructions sub-screen.
func (f *SettingsFlow) buildInstructionsMenu(hosted *HostedAgent) *domain.SettingsMenu {
	return &domain.SettingsMenu{
		Title: "💬 Personal Instructions\n\nAdd a note about yourself so " + hosted.Agent.Name + " can personalise its responses to you.\n\nTap Update to set or change your instructions.",
		Rows: []domain.SettingsRow{
			domain.Row(domain.Btn("✏️ Update Instructions", "kael_sm:instructions")),
			domain.Row(
				domain.Btn("← Back", "kael_sm:nav:main"),
				domain.Btn("✕  Close", "kael_sm:close"),
			),
		},
	}
}

// buildAppMenu constructs the per-integration landing screen. It shows a Tool
// Approvals button (with a live count) and an App Instructions button.
func (f *SettingsFlow) buildAppMenu(hosted *HostedAgent, integrationID string, approvals map[string]bool) *domain.SettingsMenu {
	integrationName := integrationID
	if intg, ok := hosted.Deps.IntegrationsByID[integrationID]; ok {
		integrationName = intg.Name
	}

	var active, total int
	for _, t := range hosted.Deps.ToolsByID {
		if t.IntegrationID == integrationID && !t.RequiresApproval {
			total++
			if approvals[t.ID] {
				active++
			}
		}
	}

	approvalLabel := fmt.Sprintf("🔧 Tool Approvals  (%d of %d active) →", active, total)

	return &domain.SettingsMenu{
		Title: "⚙️ " + integrationName,
		Rows: []domain.SettingsRow{
			domain.Row(domain.Btn(approvalLabel, "kael_sm:nav:tools:"+integrationID+":0")),
			domain.Row(domain.Btn("💬 App Instructions →", "kael_sm:nav:app_instr:"+integrationID)),
			domain.Row(
				domain.Btn("← Back", "kael_sm:nav:main"),
				domain.Btn("✕  Close", "kael_sm:close"),
			),
		},
	}
}

// buildAppInstructionsMenu constructs the per-app instructions sub-screen.
// It shows the current instructions (if any) with Update/Clear buttons, or an
// empty-state prompt when none are set.
func (f *SettingsFlow) buildAppInstructionsMenu(ctx context.Context, userID, integrationID, integrationName string) *domain.SettingsMenu {
	var instructions string
	if f.getAppInstructions != nil {
		instructions, _ = f.getAppInstructions(ctx, userID, integrationID)
	}

	backBtn := domain.Btn("← Back", "kael_sm:nav:app:"+integrationID)
	closeBtn := domain.Btn("✕  Close", "kael_sm:close")

	if instructions == "" {
		return &domain.SettingsMenu{
			Title: "💬 " + integrationName + " Instructions\n\nAdd specific instructions for how " + integrationName + " tools should behave — e.g. which calendar to default to, or preferred response format.\n\nNo instructions set yet.",
			Rows: []domain.SettingsRow{
				domain.Row(domain.Btn("✏️ Add Instructions", "kael_sm:app_instr_update:"+integrationID)),
				domain.Row(backBtn, closeBtn),
			},
		}
	}
	return &domain.SettingsMenu{
		Title: "💬 " + integrationName + " Instructions\n\n" + instructions,
		Rows: []domain.SettingsRow{
			domain.Row(
				domain.Btn("✏️ Update", "kael_sm:app_instr_update:"+integrationID),
				domain.Btn("🗑️ Clear", "kael_sm:app_instr_clear:"+integrationID),
			),
			domain.Row(backBtn, closeBtn),
		},
	}
}

// buildAccountMenu constructs the link-account sub-screen, showing either
// the link prompt or a confirmation that the account is already connected.
func (f *SettingsFlow) buildAccountMenu(ctx context.Context, userID string) *domain.SettingsMenu {
	if f.isEmailLinked(ctx, userID) {
		return &domain.SettingsMenu{
			Title: "✅ Account Linked\n\nYour account is connected — settings and history sync across platforms.",
			Rows: []domain.SettingsRow{
				domain.Row(
					domain.Btn("← Back", "kael_sm:nav:main"),
					domain.Btn("✕  Close", "kael_sm:close"),
				),
			},
		}
	}
	return &domain.SettingsMenu{
		Title: "🔗 Link Account\n\nConnect this chat to your Kael account to sync settings and history across platforms.\n\nEnter your email and we'll send a one-time code.",
		Rows: []domain.SettingsRow{
			domain.Row(domain.Btn("✉️ Enter Email to Link", "kael_sm:link_account")),
			domain.Row(
				domain.Btn("← Back", "kael_sm:nav:main"),
				domain.Btn("✕  Close", "kael_sm:close"),
			),
		},
	}
}
