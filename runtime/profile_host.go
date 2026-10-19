package runtime

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// profileSetup is everything the host needs to keep what people have shared.
type profileSetup struct {
	store   domain.ProfileDetailStore
	typed   TypedDetails
	learner DetailLearning
	migrate ProfileMigrating

	migrating sync.Map // "userID|scope" while an older note is being moved
	retryAt   sync.Map // "userID|scope" → when a failed move may be tried again
}

// migrationRetry is how long to wait before trying again to move a note that could not be moved.
const migrationRetry = 10 * time.Minute

// ProfileMigrating moves a person's older free-text note into details (see profile_migrate.go).
type ProfileMigrating interface {
	Migrate(ctx context.Context, userID, scope, legacy string) error
}

// SetProfileDetails turns on the detail maps: what an agent is told about a person comes from them,
// and typed (optional) puts the person's own fields, such as their name and time zone, in the same map.
func (h *Host) SetProfileDetails(store domain.ProfileDetailStore, typed TypedDetails) {
	h.profileDetails.store, h.profileDetails.typed = store, typed
}

// SetDetailLearner sets what learns details from finished conversations.
func (h *Host) SetDetailLearner(l DetailLearning) { h.profileDetails.learner = l }

// SetProfileMigrator sets what moves older free-text notes into details.
func (h *Host) SetProfileMigrator(m ProfileMigrating) { h.profileDetails.migrate = m }

// usedIntegrations is the integrations this agent really works with: the ones its abilities are granted
// or whose tools they use, and the ones it has an account for. It is narrower than View, which is
// everything its creator could use: what a person shared with one app is not for another agent.
func (h *HostedAgent) usedIntegrations() map[string]*domain.Integration {
	tools, identities, all := h.View()
	used := map[string]*domain.Integration{}
	add := func(id string) {
		if i, ok := all[id]; ok {
			used[id] = i
		}
	}
	for _, sk := range h.Agent.Skills {
		for _, id := range sk.Integrations {
			add(id)
		}
		for _, b := range sk.Tools {
			if t, ok := tools[b.ToolID]; ok {
				add(t.IntegrationID)
			}
		}
	}
	for _, id := range h.Agent.IdentityIDs {
		if i, ok := identities[id]; ok {
			add(i.IntegrationID)
		}
	}
	return used
}

// profileForPrompt is what this agent is told about the person: their personal details, and what they
// shared with each of the agent's own apps (and none of the others').
func (h *Host) profileForPrompt(ctx context.Context, userID string, hosted *HostedAgent) (personal string, apps map[string]string) {
	if userID == "" {
		return "", nil
	}
	used := hosted.usedIntegrations()
	ps := &h.profileDetails

	var legacyPersonal string
	if h.userProfileLoader != nil {
		if p, err := h.userProfileLoader(ctx, userID); err == nil && p != nil {
			legacyPersonal = p.Notes
		}
	}
	var legacyApps map[string]string
	if h.userIntegrationNotesLoader != nil {
		if notes, err := h.userIntegrationNotesLoader(ctx, userID); err == nil {
			legacyApps = notes
		}
	}

	if ps.store == nil {
		apps = map[string]string{}
		for id := range used {
			if n := legacyApps[id]; n != "" {
				apps[id] = n
			}
		}
		return legacyPersonal, apps
	}

	scopes := []string{domain.ProfilePersonal}
	for id := range used {
		scopes = append(scopes, id)
	}
	sort.Strings(scopes[1:])
	stored, err := ps.store.ProfileDetails(ctx, userID, scopes...)
	if err != nil {
		log.Printf("runtime: profile: load details for user %s: %v", userID, err)
	}
	by := map[string][]domain.ProfileDetail{}
	for _, d := range stored {
		by[d.Scope] = append(by[d.Scope], d)
	}
	if ps.typed != nil {
		if typed, err := ps.typed.Typed(ctx, userID); err == nil {
			by[domain.ProfilePersonal] = MergeTyped(by[domain.ProfilePersonal], typed, ps.typed)
		}
	}

	// A note written before details existed still counts until it has been moved over.
	text := func(scope, legacy string) string {
		out := domain.RenderDetails(by[scope])
		if strings.TrimSpace(legacy) == "" {
			return out
		}
		if done, err := ps.store.ProfileMigrated(ctx, userID, scope); err != nil || done {
			return out
		}
		h.startMigration(userID, scope, legacy)
		if out == "" {
			return legacy
		}
		return out + "\n" + legacy
	}
	personal = text(domain.ProfilePersonal, legacyPersonal)
	apps = map[string]string{}
	for id := range used {
		if t := text(id, legacyApps[id]); t != "" {
			apps[id] = t
		}
	}
	return personal, apps
}

// startMigration moves an older note into details in the background, once at a time per note.
func (h *Host) startMigration(userID, scope, legacy string) {
	ps := &h.profileDetails
	if ps.migrate == nil {
		return
	}
	key := userID + "|" + scope
	if at, ok := ps.retryAt.Load(key); ok && clockNow().Before(at.(time.Time)) {
		return
	}
	if _, running := ps.migrating.LoadOrStore(key, true); running {
		return
	}
	go func() {
		defer ps.migrating.Delete(key)
		if err := ps.migrate.Migrate(context.Background(), userID, scope, legacy); err != nil {
			log.Printf("runtime: profile: move older note for user %s (%s): %v", userID, scope, err)
			ps.retryAt.Store(key, clockNow().Add(migrationRetry))
			return
		}
		ps.retryAt.Delete(key)
	}()
}

// maybeLearn learns from a finished exchange, in the background, when the message is long enough to
// plausibly say something about the person. Where the messenger has buttons, the learner can ask the
// person to confirm a change; the question is asked after the reply, never before it.
func (h *Host) maybeLearn(ctx context.Context, conv domain.ConversationRef, hosted *HostedAgent, userMessage, agentResponse string) {
	ps := &h.profileDetails
	if conv.UserID == "" || ps.learner == nil || !isSubstantialMessage(userMessage) {
		return
	}
	apps := map[string]string{}
	for id, i := range hosted.usedIntegrations() {
		apps[id] = i.Name
	}
	req := LearnRequest{UserID: conv.UserID, AgentID: hosted.Agent.ID, Conv: conv, UserMessage: userMessage, AgentResponse: agentResponse, Apps: apps}
	bg := context.Background()
	if cr, ok := domain.ChoiceRequesterFromContext(ctx); ok {
		bg = domain.WithChoiceRequester(bg, cr)
	}
	go func() {
		if err := ps.learner.Learn(bg, req); err != nil {
			log.Printf("runtime: profile: user %s agent %s: %v", conv.UserID, hosted.Agent.ID, err)
		}
	}()
}

// UsedIntegrations is the integrations this agent really works with (see usedIntegrations).
func (h *HostedAgent) UsedIntegrations() map[string]*domain.Integration { return h.usedIntegrations() }

// StartProfileMigration moves an older free-text note into details in the background, unless that
// is already going on or has just failed.
func (h *Host) StartProfileMigration(userID, scope, legacy string) {
	h.startMigration(userID, scope, legacy)
}
