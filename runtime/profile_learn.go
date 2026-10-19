package runtime

// How an agent learns about a person: one named detail at a time, into a map (see
// domain/profile_details.go). Reading what was said is the model's job, and judging whether two
// names or two values mean the same is the classifier's; both sit behind small interfaces. What is
// decided here is only what happens next:
//
//   - a detail we don't hold is saved straight away;
//   - one we hold, said again the same way, is only noted as still right;
//   - one we hold, said differently, is never replaced on the strength of a conversation alone: the
//     person is asked, right after the reply, with Yes/No buttons, and only Yes changes it. No answer,
//     a new message, or a messenger without buttons all leave the old value as it is.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/unitz007/open-kael/domain"
)

const (
	// MatchThreshold is how alike two kinds of information must be to count as the same detail.
	MatchThreshold = 0.7
	// FlagThreshold and above (but under MatchThreshold) is kept as a new detail, and noted in the log
	// as a candidate for a later tidy-up.
	FlagThreshold = 0.55

	maxProposalsPerExchange = 8
	maxQuestionsPerExchange = 2
	askTimeoutSeconds       = 120
	declinedMemory          = 180 * 24 * time.Hour
)

// ValueRelation is how a newly stated value relates to the one held.
type ValueRelation int

const (
	// RelDifferent is the safe default: it is asked about, never assumed.
	RelDifferent ValueRelation = iota
	// RelSame is the same value said another way ("Germany" and "Deutschland").
	RelSame
	// RelAdds keeps the old value and adds detail to it ("Wolfsburg" → "Wolfsburg, Germany").
	RelAdds
)

// DetailProposal is a lasting fact the person stated, as the model read it.
type DetailProposal struct {
	Scope string // domain.ProfilePersonal, or the ID of an app the agent works with
	Key   string
	Value string
}

// AppDetails is what is held for one connected app.
type AppDetails struct {
	ID      string
	Name    string
	Details []domain.ProfileDetail
}

// ProposeRequest is what the model reads: the exchange and what is already known.
type ProposeRequest struct {
	UserMessage   string
	AgentResponse string
	Personal      []domain.ProfileDetail
	Apps          []AppDetails
}

// DetailProposer reads one exchange and returns the lasting facts the person stated in it. It must
// return only what they said, reuse a held key when one fits, and never guess.
type DetailProposer interface {
	ProposeDetails(ctx context.Context, req ProposeRequest) ([]DetailProposal, error)
}

// DetailJudge answers the three questions the decision rests on.
type DetailJudge interface {
	// SameKind finds, among the held details, the one holding the same kind of information as the
	// proposal, and how sure it is (0–1). An empty key means none comes close.
	SameKind(ctx context.Context, p DetailProposal, held []domain.ProfileDetail) (key string, score float64, err error)
	// Relation says how a newly stated value relates to the held one.
	Relation(ctx context.Context, key, held, stated string) (ValueRelation, error)
	// Lasting reports whether the statement is a lasting fact about the person, not something for now.
	Lasting(ctx context.Context, p DetailProposal, statement string) (bool, error)
}

// ErrInvalidDetail means a value is not acceptable for a detail kept elsewhere (a time zone that isn't one).
var ErrInvalidDetail = errors.New("profile: not a valid value")

// TypedDetails is the person's details that are kept as their own fields (name, time zone, language),
// shown in the same map. Owned keys are written there, not into the map's table.
type TypedDetails interface {
	// Typed returns the person's details as map entries: the owned keys and anything else derived
	// from their fields (such as where they live). It is read-only and shown in the personal map.
	Typed(ctx context.Context, userID string) ([]domain.ProfileDetail, error)
	// Owns reports whether the key is kept as a field.
	Owns(key string) bool
	// Valid reports whether a value could be kept for an owned key (a time zone must be a real one).
	Valid(key, value string) bool
	// SetTyped writes an owned key, or returns ErrInvalidDetail.
	SetTyped(ctx context.Context, userID, key, value string) error
}

// LearnRequest is one finished exchange to learn from.
type LearnRequest struct {
	UserID, AgentID string
	Conv            domain.ConversationRef
	UserMessage     string
	AgentResponse   string
	Apps            map[string]string // the apps this agent works with: ID → name
}

// DetailLearning is what the host calls after an exchange.
type DetailLearning interface {
	Learn(ctx context.Context, req LearnRequest) error
}

// DetailLearner learns details from conversations.
type DetailLearner struct {
	Store    domain.ProfileDetailStore
	Proposer DetailProposer
	Judge    DetailJudge
	Typed    TypedDetails // optional
	Now      func() time.Time

	locks sync.Map // userID → *sync.Mutex: one person's learning is one thing at a time
}

var _ DetailLearning = (*DetailLearner)(nil)

// What is never learned, whatever the model proposes: limits and permissions are set by the person
// on purpose, not picked up from a remark.
var neverLearnedKey = regexp.MustCompile(`(^|_)(permission|permissions|allow|allowed|disallow|limit|limits|consent|approval|approve|authori[sz]e|authori[sz]ation|autonomy|may|must|never|always)(_|$)`)

// NeverLearned reports whether a detail must not be kept, whatever the source: a secret, or a
// permission or limit (those are set by the person on purpose, not picked up from a remark).
func NeverLearned(key, value string) bool {
	return domain.LooksSensitive(key, value) || neverLearnedKey.MatchString(key)
}

func (l *DetailLearner) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *DetailLearner) lockFor(userID string) *sync.Mutex {
	m, _ := l.locks.LoadOrStore(userID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// state is what one exchange works on.
type learnState struct {
	req       LearnRequest
	held      map[string][]domain.ProfileDetail // scope → details, kept current as the exchange goes
	requester domain.ChoiceRequester
	questions int
}

func (st *learnState) find(scope, key string) (domain.ProfileDetail, bool) {
	for _, d := range st.held[scope] {
		if d.Key == key {
			return d, true
		}
	}
	return domain.ProfileDetail{}, false
}

func (st *learnState) put(d domain.ProfileDetail) {
	list := st.held[d.Scope]
	for i := range list {
		if list[i].Key == d.Key {
			list[i].Value, list[i].Source = d.Value, d.Source
			return
		}
	}
	st.held[d.Scope] = append(list, d)
}

// Learn reads the exchange and applies what it says to the person's maps.
func (l *DetailLearner) Learn(ctx context.Context, req LearnRequest) error {
	if req.UserID == "" || l.Store == nil || l.Proposer == nil || l.Judge == nil {
		return nil
	}
	mu := l.lockFor(req.UserID)
	mu.Lock()
	defer mu.Unlock()

	st := &learnState{req: req, held: map[string][]domain.ProfileDetail{}}
	st.requester, _ = domain.ChoiceRequesterFromContext(ctx)

	scopes := []string{domain.ProfilePersonal}
	appIDs := make([]string, 0, len(req.Apps))
	for id := range req.Apps {
		appIDs = append(appIDs, id)
	}
	sort.Strings(appIDs)
	scopes = append(scopes, appIDs...)

	stored, err := l.Store.ProfileDetails(ctx, req.UserID, scopes...)
	if err != nil {
		return fmt.Errorf("profile: load: %w", err)
	}
	for _, d := range stored {
		st.held[d.Scope] = append(st.held[d.Scope], d)
	}
	if l.Typed != nil {
		typed, err := l.Typed.Typed(ctx, req.UserID)
		if err != nil {
			return fmt.Errorf("profile: load typed: %w", err)
		}
		st.held[domain.ProfilePersonal] = mergeTyped(st.held[domain.ProfilePersonal], typed, l.Typed)
	}

	pr := ProposeRequest{UserMessage: req.UserMessage, AgentResponse: req.AgentResponse, Personal: append([]domain.ProfileDetail(nil), st.held[domain.ProfilePersonal]...)}
	for _, id := range appIDs {
		pr.Apps = append(pr.Apps, AppDetails{ID: id, Name: req.Apps[id], Details: append([]domain.ProfileDetail(nil), st.held[id]...)})
	}
	proposals, err := l.Proposer.ProposeDetails(ctx, pr)
	if err != nil {
		return fmt.Errorf("profile: read: %w", err)
	}
	if len(proposals) > maxProposalsPerExchange {
		proposals = proposals[:maxProposalsPerExchange]
	}
	for _, p := range proposals {
		l.consider(ctx, st, p)
	}
	return nil
}

// mergeTyped shows the typed details in the personal map: an owned key always comes from the typed
// field; any other key from the typed side only fills a gap the map has.
func mergeTyped(stored, typed []domain.ProfileDetail, t TypedDetails) []domain.ProfileDetail {
	out := make([]domain.ProfileDetail, 0, len(stored)+len(typed))
	have := map[string]bool{}
	for _, d := range typed {
		if t.Owns(d.Key) {
			d.Scope = domain.ProfilePersonal
			out = append(out, d)
			have[d.Key] = true
		}
	}
	for _, d := range stored {
		if !have[d.Key] {
			out = append(out, d)
			have[d.Key] = true
		}
	}
	for _, d := range typed {
		if !have[d.Key] {
			d.Scope = domain.ProfilePersonal
			out = append(out, d)
			have[d.Key] = true
		}
	}
	return out
}

// consider decides what to do with one proposed detail.
func (l *DetailLearner) consider(ctx context.Context, st *learnState, p DetailProposal) {
	user := st.req.UserID
	scope := strings.TrimSpace(p.Scope)
	if scope == "" {
		scope = domain.ProfilePersonal
	}
	if scope != domain.ProfilePersonal {
		if _, ok := st.req.Apps[scope]; !ok {
			return // only the apps this agent works with
		}
	}
	key, ok := domain.CanonicalDetailKey(p.Key)
	value, vok := domain.CleanDetailValue(p.Value)
	if !ok || !vok {
		return
	}
	if NeverLearned(key, value) {
		return
	}
	prop := DetailProposal{Scope: scope, Key: key, Value: value}

	// Is it a detail we already hold, under this name or another?
	if _, held := st.find(scope, key); !held && len(st.held[scope]) > 0 {
		match, score, err := l.Judge.SameKind(ctx, prop, st.held[scope])
		switch {
		case err != nil:
			log.Printf("profile: user %s: could not compare %q with what is known: %v", user, key, err)
			return // learn it another time rather than risk a duplicate
		case match != "" && score >= MatchThreshold:
			if _, ok := st.find(scope, match); ok {
				key, prop.Key = match, match
			}
		case match != "" && score >= FlagThreshold:
			log.Printf("profile: user %s: %q looks close to %q (%.2f), kept apart; a candidate for tidying up", user, key, match, score)
		}
	}

	old, held := st.find(scope, key)
	if !held {
		l.learnNew(ctx, st, prop)
		return
	}
	if domain.SameDetailValue(old.Value, value) {
		_ = l.Store.ConfirmProfileDetail(ctx, user, scope, key)
		return
	}
	rel, err := l.Judge.Relation(ctx, key, old.Value, value)
	if err != nil {
		log.Printf("profile: user %s: could not compare values for %q: %v", user, key, err)
		return
	}
	if rel == RelSame {
		_ = l.Store.ConfirmProfileDetail(ctx, user, scope, key)
		return
	}
	l.proposeChange(ctx, st, prop, old, rel)
}

// lasting asks whether a statement is worth keeping; if it can't be told, it isn't kept.
func (l *DetailLearner) lasting(ctx context.Context, st *learnState, p DetailProposal) bool {
	ok, err := l.Judge.Lasting(ctx, p, st.req.UserMessage)
	if err != nil {
		log.Printf("profile: user %s: could not tell whether %q is lasting: %v", st.req.UserID, p.Key, err)
		return false
	}
	return ok
}

// learnNew saves a detail we didn't hold.
func (l *DetailLearner) learnNew(ctx context.Context, st *learnState, p DetailProposal) {
	if !l.lasting(ctx, st, p) {
		return
	}
	if err := l.write(ctx, st.req.UserID, p, domain.DetailSaid); err != nil {
		if !errors.Is(err, domain.ErrProfileFull) && !errors.Is(err, ErrInvalidDetail) {
			log.Printf("profile: user %s: could not save %q: %v", st.req.UserID, p.Key, err)
		}
		return
	}
	st.put(domain.ProfileDetail{Scope: p.Scope, Key: p.Key, Value: p.Value, Source: domain.DetailSaid})
}

// write puts a detail where it belongs: its own field for the owned keys, the map otherwise.
func (l *DetailLearner) write(ctx context.Context, userID string, p DetailProposal, source string) error {
	if p.Scope == domain.ProfilePersonal && l.Typed != nil && l.Typed.Owns(p.Key) {
		return l.Typed.SetTyped(ctx, userID, p.Key, p.Value)
	}
	return l.Store.SetProfileDetail(ctx, userID, domain.ProfileDetail{Scope: p.Scope, Key: p.Key, Value: p.Value, Source: source})
}

// proposeChange asks the person before a held detail is changed. Nothing changes unless they say yes.
func (l *DetailLearner) proposeChange(ctx context.Context, st *learnState, p DetailProposal, old domain.ProfileDetail, rel ValueRelation) {
	user := st.req.UserID
	if p.Scope == domain.ProfilePersonal && l.Typed != nil && l.Typed.Owns(p.Key) && !l.Typed.Valid(p.Key, p.Value) {
		return // never even ask about a value that couldn't be kept
	}
	if refused, err := l.Store.DeclinedProfileValue(ctx, user, p.Scope, p.Key, p.Value, l.now().Add(-declinedMemory)); err == nil && refused {
		return // they said no to this already, in whatever words
	}
	if !l.lasting(ctx, st, p) {
		return
	}
	if st.requester == nil {
		log.Printf("profile: user %s: %q differs from what is known, and there is no way to ask here; keeping what is known", user, p.Key)
		return
	}
	if st.questions >= maxQuestionsPerExchange {
		return
	}
	st.questions++

	question, yes := changeQuestion(st.req.Apps[p.Scope], p.Key, old.Value, p.Value, rel)
	ans, err := st.requester.RequestChoice(ctx, st.req.Conv, question, []string{yes, "No, keep it"}, askTimeoutSeconds)
	switch {
	case err != nil, ans.Index == domain.ChoiceSuperseded:
		return // no answer is not a yes, and not a no either
	case ans.Index == 0:
		if err := l.write(ctx, user, p, domain.DetailChanged); err != nil {
			log.Printf("profile: user %s: could not change %q: %v", user, p.Key, err)
			return
		}
		st.put(domain.ProfileDetail{Scope: p.Scope, Key: p.Key, Value: p.Value, Source: domain.DetailChanged})
	default:
		if err := l.Store.DeclineProfileValue(ctx, user, p.Scope, p.Key, p.Value); err != nil {
			log.Printf("profile: user %s: could not remember the refusal of %q: %v", user, p.Key, err)
		}
	}
}

// changeQuestion words the question and its Yes button.
func changeQuestion(appName, key, old, stated string, rel ValueRelation) (question, yes string) {
	what := strings.ReplaceAll(key, "_", " ")
	prefix := ""
	if appName != "" {
		prefix = appName + ": "
	}
	if rel == RelAdds {
		return fmt.Sprintf("%sI have your %s as “%s”. Add the detail, “%s”?", prefix, what, old, stated), "Yes, add it"
	}
	return fmt.Sprintf("%sI have your %s as “%s”. Update it to “%s”?", prefix, what, old, stated), "Yes, update it"
}
