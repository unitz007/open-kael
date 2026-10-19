package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type memStore struct {
	mu       sync.Mutex
	details  map[string]domain.ProfileDetail // scope|key
	declined map[string]bool
	migrated map[string]bool
	confirms []string
	full     bool
}

func newMemStore() *memStore {
	return &memStore{details: map[string]domain.ProfileDetail{}, declined: map[string]bool{}, migrated: map[string]bool{}}
}

func (m *memStore) ProfileDetails(_ context.Context, _ string, scopes ...string) ([]domain.ProfileDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.ProfileDetail
	for _, d := range m.details {
		if len(scopes) == 0 {
			out = append(out, d)
			continue
		}
		for _, s := range scopes {
			if s == d.Scope {
				out = append(out, d)
			}
		}
	}
	return out, nil
}
func (m *memStore) SetProfileDetail(_ context.Context, _ string, d domain.ProfileDetail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.full {
		return domain.ErrProfileFull
	}
	m.details[d.Scope+"|"+d.Key] = d
	return nil
}
func (m *memStore) ConfirmProfileDetail(_ context.Context, _, scope, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirms = append(m.confirms, scope+"|"+key)
	return nil
}
func (m *memStore) RemoveProfileDetail(_ context.Context, _, scope, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.details[scope+"|"+key]
	delete(m.details, scope+"|"+key)
	return ok, nil
}
func (m *memStore) DeclineProfileValue(_ context.Context, _, scope, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.declined[scope+"|"+key+"|"+domain.ComparableValue(value)] = true
	return nil
}
func (m *memStore) DeclinedProfileValue(_ context.Context, _, scope, key, value string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.declined[scope+"|"+key+"|"+domain.ComparableValue(value)], nil
}
func (m *memStore) ProfileMigrated(_ context.Context, _, scope string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.migrated[scope], nil
}
func (m *memStore) MarkProfileMigrated(_ context.Context, _, scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.migrated[scope] = true
	return nil
}
func (m *memStore) value(scope, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.details[scope+"|"+key].Value
}
func (m *memStore) seed(scope, key, value string) {
	m.details[scope+"|"+key] = domain.ProfileDetail{Scope: scope, Key: key, Value: value, Source: domain.DetailSaid}
}

type fixedProposer struct {
	props []DetailProposal
	err   error
	got   ProposeRequest
}

func (f *fixedProposer) ProposeDetails(_ context.Context, r ProposeRequest) ([]DetailProposal, error) {
	f.got = r
	return f.props, f.err
}

type tableJudge struct {
	kind     map[string]string        // proposed key → held key
	score    map[string]float64       // proposed key → score (default 0.9 when matched)
	relation map[string]ValueRelation // proposed value → relation
	temp     map[string]bool          // proposed key → not lasting
	kindErr  error
	lastErr  error
	calls    struct{ kind, relation, lasting int }
}

func (j *tableJudge) SameKind(_ context.Context, p DetailProposal, _ []domain.ProfileDetail) (string, float64, error) {
	j.calls.kind++
	if j.kindErr != nil {
		return "", 0, j.kindErr
	}
	k, ok := j.kind[p.Key]
	if !ok {
		return "", 0.1, nil
	}
	if s, ok := j.score[p.Key]; ok {
		return k, s, nil
	}
	return k, 0.9, nil
}
func (j *tableJudge) Relation(_ context.Context, _, _, stated string) (ValueRelation, error) {
	j.calls.relation++
	return j.relation[stated], nil
}
func (j *tableJudge) Lasting(_ context.Context, p DetailProposal, _ string) (bool, error) {
	j.calls.lasting++
	if j.lastErr != nil {
		return false, j.lastErr
	}
	return !j.temp[p.Key], nil
}

type buttons struct {
	mu      sync.Mutex
	answers []domain.ChoiceAnswer // consumed in order
	err     error
	asked   []string
	options [][]string
	delay   time.Duration
}

func (b *buttons) RequestChoice(_ context.Context, _ domain.ConversationRef, q string, opts []string, _ int) (domain.ChoiceAnswer, error) {
	time.Sleep(b.delay) // a person takes a moment to answer
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, q)
	b.options = append(b.options, opts)
	if b.err != nil {
		return domain.ChoiceAnswer{}, b.err
	}
	if len(b.answers) == 0 {
		return domain.ChoiceAnswer{Index: 0}, nil
	}
	a := b.answers[0]
	b.answers = b.answers[1:]
	return a, nil
}

type fakeTyped struct {
	vals map[string]string
	set  []string
}

func (f *fakeTyped) Typed(context.Context, string) ([]domain.ProfileDetail, error) {
	var out []domain.ProfileDetail
	for k, v := range f.vals {
		out = append(out, domain.ProfileDetail{Scope: domain.ProfilePersonal, Key: k, Value: v, Source: domain.DetailSaid})
	}
	return out, nil
}
func (f *fakeTyped) Owns(k string) bool { return k == "name" || k == "time_zone" || k == "language" }
func (f *fakeTyped) Valid(k, v string) bool {
	if k == "time_zone" {
		_, err := time.LoadLocation(v)
		return err == nil && v != ""
	}
	return v != ""
}
func (f *fakeTyped) SetTyped(_ context.Context, _, k, v string) error {
	if !f.Valid(k, v) {
		return ErrInvalidDetail
	}
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	f.vals[k] = v
	f.set = append(f.set, k+"="+v)
	return nil
}

func learner(st *memStore, props []DetailProposal, j *tableJudge) (*DetailLearner, *fixedProposer) {
	p := &fixedProposer{props: props}
	if j == nil {
		j = &tableJudge{}
	}
	return &DetailLearner{Store: st, Proposer: p, Judge: j}, p
}

func learn(t *testing.T, l *DetailLearner, b *buttons, apps map[string]string) {
	t.Helper()
	ctx := context.Background()
	if b != nil {
		ctx = domain.WithChoiceRequester(ctx, b)
	}
	if err := l.Learn(ctx, LearnRequest{UserID: "u1", AgentID: "a1", UserMessage: "I live in Wolfsburg, Germany", Apps: apps}); err != nil {
		t.Fatal(err)
	}
}

const P = domain.ProfilePersonal

// ── tests ────────────────────────────────────────────────────────────────────

func TestLearn_NewDetailIsSavedAtOnce_NoQuestion(t *testing.T) {
	st := newMemStore()
	l, _ := learner(st, []DetailProposal{{Key: "Country", Value: "Nigeria"}, {Key: "residence", Value: "Wolfsburg, Germany"}}, nil)
	b := &buttons{}
	learn(t, l, b, nil)
	if st.value(P, "country") != "Nigeria" || st.value(P, "residence") != "Wolfsburg, Germany" {
		t.Errorf("saved: %+v", st.details)
	}
	if st.details[P+"|country"].Source != domain.DetailSaid || len(b.asked) != 0 {
		t.Errorf("said, and nobody was bothered: %+v %v", st.details[P+"|country"], b.asked)
	}
}

func TestLearn_SameValueOnlyRefreshes(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg, Germany")
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "wolfsburg germany"}}, nil)
	b := &buttons{}
	learn(t, l, b, nil)
	if st.value(P, "residence") != "Wolfsburg, Germany" || len(st.confirms) != 1 || len(b.asked) != 0 {
		t.Errorf("value untouched, noted as still right, no question: %v %v %v", st.value(P, "residence"), st.confirms, b.asked)
	}
}

func TestLearn_JudgeSaysSameValueInOtherWords_Refreshes(t *testing.T) {
	st := newMemStore()
	st.seed(P, "country", "Germany")
	j := &tableJudge{relation: map[string]ValueRelation{"Deutschland": RelSame}}
	l, _ := learner(st, []DetailProposal{{Key: "country", Value: "Deutschland"}}, j)
	b := &buttons{}
	learn(t, l, b, nil)
	if st.value(P, "country") != "Germany" || len(b.asked) != 0 || len(st.confirms) != 1 {
		t.Errorf("%v %v %v", st.value(P, "country"), b.asked, st.confirms)
	}
}

func TestLearn_DifferentValue_AsksAndOnlyYesChanges(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg, Germany")
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "Lagos, Nigeria"}}, nil)

	// Yes
	b := &buttons{answers: []domain.ChoiceAnswer{{Index: 0}}}
	learn(t, l, b, nil)
	if len(b.asked) != 1 || !strings.Contains(b.asked[0], "Wolfsburg, Germany") || !strings.Contains(b.asked[0], "Lagos, Nigeria") || !strings.Contains(b.asked[0], "residence") {
		t.Errorf("the question names the detail, the old and the new: %v", b.asked)
	}
	if b.options[0][0] != "Yes, update it" || b.options[0][1] != "No, keep it" {
		t.Errorf("buttons: %v", b.options[0])
	}
	if st.value(P, "residence") != "Lagos, Nigeria" || st.details[P+"|residence"].Source != domain.DetailChanged {
		t.Errorf("changed after yes: %+v", st.details[P+"|residence"])
	}
}

func TestLearn_No_KeepsAndRemembersTheRefusal(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg, Germany")
	l, p := learner(st, []DetailProposal{{Key: "residence", Value: "Lagos, Nigeria"}}, nil)
	b := &buttons{answers: []domain.ChoiceAnswer{{Index: 1}}}
	learn(t, l, b, nil)
	if st.value(P, "residence") != "Wolfsburg, Germany" || len(b.asked) != 1 {
		t.Fatalf("kept: %v %v", st.value(P, "residence"), b.asked)
	}
	// The same value again, in other words, is not asked again.
	p.props = []DetailProposal{{Key: "lives_in", Value: "lagos  NIGERIA"}}
	learn(t, l, b, nil)
	if len(b.asked) != 1 || st.value(P, "residence") != "Wolfsburg, Germany" {
		t.Errorf("not asked twice: %v", b.asked)
	}
	// A different new value is a new question.
	p.props = []DetailProposal{{Key: "residence", Value: "Abuja"}}
	b.answers = []domain.ChoiceAnswer{{Index: 2}} // "something else" is a no too
	learn(t, l, b, nil)
	if len(b.asked) != 2 || st.value(P, "residence") != "Wolfsburg, Germany" {
		t.Errorf("%v", b.asked)
	}
	p.props = []DetailProposal{{Key: "residence", Value: "abuja"}}
	learn(t, l, b, nil)
	if len(b.asked) != 2 {
		t.Errorf("something else counts as no: %v", b.asked)
	}
}

func TestLearn_NoAnswerIsNeitherYesNorNo(t *testing.T) {
	for name, b := range map[string]*buttons{
		"timeout":    {err: context.DeadlineExceeded},
		"superseded": {answers: []domain.ChoiceAnswer{{Index: domain.ChoiceSuperseded}}},
	} {
		st := newMemStore()
		st.seed(P, "residence", "Wolfsburg")
		l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "Lagos"}}, nil)
		learn(t, l, b, nil)
		if st.value(P, "residence") != "Wolfsburg" {
			t.Errorf("%s: changed without a yes", name)
		}
		if len(st.declined) != 0 {
			t.Errorf("%s: an ignored question is not a refusal: %v", name, st.declined)
		}
	}
}

func TestLearn_NoWayToAsk_KeepsOldButStillLearnsNew(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "Lagos"}, {Key: "country", Value: "Nigeria"}}, nil)
	learn(t, l, nil, nil) // no buttons here
	if st.value(P, "residence") != "Wolfsburg" || st.value(P, "country") != "Nigeria" {
		t.Errorf("%+v", st.details)
	}
}

func TestLearn_RefinementIsWordedAsAddingDetail(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	j := &tableJudge{relation: map[string]ValueRelation{"Wolfsburg, Germany": RelAdds}}
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "Wolfsburg, Germany"}}, j)
	b := &buttons{}
	learn(t, l, b, nil)
	if len(b.asked) != 1 || !strings.Contains(b.asked[0], "Add the detail") || b.options[0][0] != "Yes, add it" {
		t.Errorf("%v %v", b.asked, b.options)
	}
	if st.value(P, "residence") != "Wolfsburg, Germany" {
		t.Errorf("added after yes: %q", st.value(P, "residence"))
	}
}

func TestLearn_OtherNamesForAHeldDetailAreThatDetail(t *testing.T) {
	// By the synonym list.
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	l, _ := learner(st, []DetailProposal{{Key: "Lives In", Value: "Lagos"}}, nil)
	b := &buttons{answers: []domain.ChoiceAnswer{{Index: 1}}}
	learn(t, l, b, nil)
	if len(st.details) != 1 || len(b.asked) != 1 {
		t.Errorf("no second detail; asked about the first: %+v %v", st.details, b.asked)
	}

	// By the classifier, at or above the threshold.
	st = newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	j := &tableJudge{kind: map[string]string{"home_town": "residence"}, score: map[string]float64{"home_town": 0.7}}
	l, _ = learner(st, []DetailProposal{{Key: "home_town", Value: "Lagos"}}, j)
	b = &buttons{answers: []domain.ChoiceAnswer{{Index: 1}}}
	learn(t, l, b, nil)
	if _, dup := st.details[P+"|home_town"]; dup || len(b.asked) != 1 {
		t.Errorf("same kind: %+v %v", st.details, b.asked)
	}

	// Just under: kept as its own detail.
	st = newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	j = &tableJudge{kind: map[string]string{"home_town": "residence"}, score: map[string]float64{"home_town": 0.69}}
	l, _ = learner(st, []DetailProposal{{Key: "home_town", Value: "Lagos"}}, j)
	b = &buttons{}
	learn(t, l, b, nil)
	if st.value(P, "home_town") != "Lagos" || len(b.asked) != 0 {
		t.Errorf("under the threshold is its own detail: %+v", st.details)
	}
}

func TestLearn_AtMostTwoQuestionsPerExchange(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "A")
	st.seed(P, "country", "B")
	st.seed(P, "occupation", "C")
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "A2"}, {Key: "country", Value: "B2"}, {Key: "occupation", Value: "C2"}, {Key: "hobbies", Value: "chess"}}, nil)
	b := &buttons{}
	learn(t, l, b, nil)
	if len(b.asked) != 2 {
		t.Fatalf("questions: %v", b.asked)
	}
	if st.value(P, "occupation") != "C" {
		t.Errorf("the third is not saved over what we have: %q", st.value(P, "occupation"))
	}
	if st.value(P, "hobbies") != "chess" {
		t.Errorf("new details need no question and are still saved")
	}
}

func TestLearn_OnlyLastingFactsAreKept(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	j := &tableJudge{temp: map[string]bool{"mood": true, "residence": true}}
	l, _ := learner(st, []DetailProposal{{Key: "mood", Value: "tired today"}, {Key: "residence", Value: "a hotel in Paris tonight"}}, j)
	b := &buttons{}
	learn(t, l, b, nil)
	if _, ok := st.details[P+"|mood"]; ok || st.value(P, "residence") != "Wolfsburg" || len(b.asked) != 0 {
		t.Errorf("a passing remark neither saved nor asked about: %+v %v", st.details, b.asked)
	}
}

func TestLearn_NeverKeepsSecretsLimitsOrPermissions(t *testing.T) {
	st := newMemStore()
	l, _ := learner(st, []DetailProposal{
		{Key: "password", Value: "hunter2"}, {Key: "note", Value: "card 4111 1111 1111 1111"},
		{Key: "trading_limit", Value: "5000"}, {Key: "permissions", Value: "may trade freely"}, {Key: "allowed_actions", Value: "all"},
		{Key: "favourite_club", Value: "Arsenal"},
	}, nil)
	learn(t, l, nil, nil)
	if len(st.details) != 1 || st.value(P, "favourite_club") != "Arsenal" {
		t.Errorf("only the harmless one: %+v", st.details)
	}
}

func TestLearn_OnlyAppsTheAgentWorksWith(t *testing.T) {
	st := newMemStore()
	l, p := learner(st, []DetailProposal{{Scope: "int_fpl", Key: "squad_name", Value: "Gunners FC"}, {Scope: "int_other", Key: "other_thing", Value: "y"}}, nil)
	learn(t, l, nil, map[string]string{"int_fpl": "Fantasy Football"})
	if st.value("int_other", "other_thing") != "" {
		t.Errorf("not an app this agent works with: %+v", st.details)
	}
	if st.value("int_fpl", "squad_name") != "Gunners FC" {
		t.Errorf("its own app is learned: %+v", st.details)
	}
	if len(p.got.Apps) != 1 || p.got.Apps[0].Name != "Fantasy Football" {
		t.Errorf("the model is shown only that app's details: %+v", p.got.Apps)
	}
}

func TestLearn_AppQuestionNamesTheApp(t *testing.T) {
	st := newMemStore()
	st.seed("int_fpl", "favourite_club", "Arsenal")
	l, _ := learner(st, []DetailProposal{{Scope: "int_fpl", Key: "favourite_club", Value: "Chelsea"}}, nil)
	b := &buttons{}
	learn(t, l, b, map[string]string{"int_fpl": "Fantasy Football"})
	if len(b.asked) != 1 || !strings.HasPrefix(b.asked[0], "Fantasy Football: ") {
		t.Errorf("%v", b.asked)
	}
}

func TestLearn_OwnedKeysGoToTheirFields(t *testing.T) {
	st := newMemStore()
	ty := &fakeTyped{vals: map[string]string{"time_zone": "Europe/Berlin"}}
	l, _ := learner(st, []DetailProposal{{Key: "name", Value: "Charles Dinneya"}, {Key: "timezone", Value: "Not/AZone"}, {Key: "language", Value: "English"}}, nil)
	l.Typed = ty
	b := &buttons{}
	learn(t, l, b, nil)
	if ty.vals["name"] != "Charles Dinneya" || ty.vals["language"] != "English" || len(st.details) != 0 {
		t.Errorf("name and language into their fields, not the map: %v %+v", ty.vals, st.details)
	}
	if ty.vals["time_zone"] != "Europe/Berlin" || len(b.asked) != 0 {
		t.Errorf("an invalid zone is dropped without a question: %v %v", ty.vals, b.asked)
	}

	// A different valid zone is asked about; yes writes it to the field.
	l.Proposer.(*fixedProposer).props = []DetailProposal{{Key: "time_zone", Value: "Africa/Lagos"}}
	learn(t, l, b, nil)
	if len(b.asked) != 1 || ty.vals["time_zone"] != "Africa/Lagos" {
		t.Errorf("%v %v", b.asked, ty.vals)
	}
}

func TestLearn_TypedDetailsFillGapsAndAreShownToTheModel(t *testing.T) {
	st := newMemStore()
	ty := &fakeTyped{vals: map[string]string{"residence": "Wolfsburg, Germany"}}
	l, p := learner(st, []DetailProposal{{Key: "residence", Value: "Lagos"}}, nil)
	l.Typed = ty
	b := &buttons{answers: []domain.ChoiceAnswer{{Index: 0}}}
	learn(t, l, b, nil)
	if len(p.got.Personal) != 1 || p.got.Personal[0].Value != "Wolfsburg, Germany" {
		t.Errorf("shown: %+v", p.got.Personal)
	}
	if len(b.asked) != 1 || st.value(P, "residence") != "Lagos" {
		t.Errorf("derived value is confirmed before being replaced, then stored in the map: %v %+v", b.asked, st.details)
	}
}

func TestLearn_FullMapAndBrokenHelpersNeverCorrupt(t *testing.T) {
	st := newMemStore()
	st.full = true
	l, _ := learner(st, []DetailProposal{{Key: "hobbies", Value: "chess"}}, nil)
	learn(t, l, nil, nil)
	if len(st.details) != 0 {
		t.Error("a full map takes nothing new")
	}

	st = newMemStore()
	st.seed(P, "residence", "Wolfsburg")
	j := &tableJudge{kindErr: errors.New("down")}
	l, _ = learner(st, []DetailProposal{{Key: "home_town", Value: "Lagos"}}, j)
	learn(t, l, nil, nil)
	if len(st.details) != 1 {
		t.Errorf("if the comparison fails the detail is left for another time, not risked as a duplicate: %+v", st.details)
	}

	st = newMemStore()
	j = &tableJudge{lastErr: errors.New("down")}
	l, _ = learner(st, []DetailProposal{{Key: "hobbies", Value: "chess"}}, j)
	learn(t, l, nil, nil)
	if len(st.details) != 0 {
		t.Errorf("if lasting can't be told, nothing is kept: %+v", st.details)
	}

	l, p := learner(newMemStore(), nil, nil)
	p.err = errors.New("model down")
	if err := l.Learn(context.Background(), LearnRequest{UserID: "u1"}); err == nil {
		t.Error("a failed read is reported")
	}
	if err := (&DetailLearner{}).Learn(context.Background(), LearnRequest{UserID: "u1"}); err != nil {
		t.Errorf("not set up is not an error: %v", err)
	}
}

func TestLearn_ProposalsAreCapped(t *testing.T) {
	st := newMemStore()
	var props []DetailProposal
	for i := 0; i < 30; i++ {
		props = append(props, DetailProposal{Key: "k" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Value: "v"})
	}
	l, _ := learner(st, props, nil)
	learn(t, l, nil, nil)
	if len(st.details) != maxProposalsPerExchange {
		t.Errorf("one exchange teaches only so much: %d", len(st.details))
	}
}

func TestLearn_OnePersonAtATime(t *testing.T) {
	st := newMemStore()
	st.seed(P, "residence", "A")
	l, _ := learner(st, []DetailProposal{{Key: "residence", Value: "B"}}, nil)
	b := &buttons{delay: 30 * time.Millisecond}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); learn(t, l, b, nil) }()
	}
	wg.Wait()
	if len(b.asked) != 1 {
		t.Errorf("after the first yes the rest see the new value and have nothing to ask: %d questions", len(b.asked))
	}
}

func TestNeverLearned(t *testing.T) {
	for _, c := range []struct {
		k, v string
		want bool
	}{
		{"password", "x", true}, {"daily_limit", "5000", true}, {"permissions", "all", true}, {"note", "4111 1111 1111 1111", true},
		{"residence", "Wolfsburg", false}, {"favourite_club", "Arsenal", false}, {"mayor", "x", false}, {"allowance", "x", false},
	} {
		if got := NeverLearned(c.k, c.v); got != c.want {
			t.Errorf("%s=%s: %v", c.k, c.v, got)
		}
	}
}
