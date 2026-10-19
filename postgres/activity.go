package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/unitz007/open-kael/domain"
)

// Activity: how an agent is doing, as counts (see domain.ActivityRecorder). Nothing stored here
// holds what anyone wrote or who they are.

var (
	_ domain.ActivityRecorder = (*Store)(nil)
	_ domain.ActivityReader   = (*Store)(nil)
)

const (
	activitySaltKey   = "activity_salt"
	activityKeepDays  = 35
	activityMaxEvents = 5000 // per agent
	pruneEvery        = time.Hour
)

// activityState is what the store remembers between calls, so recording stays cheap.
type activityState struct {
	mu        sync.Mutex
	salt      []byte
	seen      map[string]struct{} // agent|code|day already written today
	seenDay   string
	lastPrune time.Time
}

// activitySalt is the platform secret the person codes are made with, created once.
func (s *Store) activitySalt(ctx context.Context) ([]byte, error) {
	s.act.mu.Lock()
	defer s.act.mu.Unlock()
	if s.act.salt != nil {
		return s.act.salt, nil
	}
	v, err := s.GetPlatformConfig(ctx, activitySaltKey)
	if errors.Is(err, domain.ErrNotFound) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		v = hex.EncodeToString(raw)
		// Two servers starting at once must agree on one secret: the first write wins.
		if _, err := s.pool.Exec(ctx, `INSERT INTO platform_config (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, activitySaltKey, v); err != nil {
			return nil, err
		}
		v, err = s.GetPlatformConfig(ctx, activitySaltKey)
	}
	if err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(v)
	if err != nil || len(salt) == 0 {
		return nil, fmt.Errorf("activity: unreadable secret")
	}
	s.act.salt = salt
	return salt, nil
}

func personCode(salt []byte, person string) string {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(person))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// PersonActive implements domain.ActivityRecorder.
func (s *Store) PersonActive(ctx context.Context, agentID, person string) {
	if agentID == "" || person == "" {
		return
	}
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	salt, err := s.activitySalt(ctx)
	if err != nil {
		log.Printf("activity: %v", err)
		return
	}
	code := personCode(salt, person)

	key := agentID + "|" + code
	s.act.mu.Lock()
	if s.act.seenDay != day {
		s.act.seen, s.act.seenDay = map[string]struct{}{}, day
	}
	if _, done := s.act.seen[key]; done {
		s.act.mu.Unlock()
		return
	}
	s.act.mu.Unlock()

	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_activity_people (agent_id, day, person) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, agentID, day, code); err != nil {
		log.Printf("activity: note a person: %v", err)
		return
	}
	s.act.mu.Lock()
	s.act.seen[key] = struct{}{}
	s.act.mu.Unlock()
	s.pruneActivity(ctx, now)
}

// MessageHandled implements domain.ActivityRecorder: one more message in this minute.
func (s *Store) MessageHandled(ctx context.Context, agentID string) {
	if agentID == "" {
		return
	}
	minute := time.Now().UTC().Truncate(time.Minute)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO agent_activity_minutes (agent_id, minute, messages) VALUES ($1, $2, 1)
		ON CONFLICT (agent_id, minute) DO UPDATE SET messages = agent_activity_minutes.messages + 1`, agentID, minute); err != nil {
		log.Printf("activity: count a message: %v", err)
		return
	}
	s.pruneActivity(ctx, minute)
}

// ActivityEvent implements domain.ActivityRecorder.
func (s *Store) ActivityEvent(ctx context.Context, agentID string, kind domain.ActivityKind, detail string) {
	if agentID == "" || kind == "" {
		return
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO agent_activity_events (agent_id, kind, detail) VALUES ($1, $2, $3)`, agentID, string(kind), domain.CleanActivityDetail(detail)); err != nil {
		log.Printf("activity: note an event: %v", err)
	}
}

// pruneActivity removes what is older than the keep period, and an agent's oldest events beyond a
// cap. It runs at most once an hour.
func (s *Store) pruneActivity(ctx context.Context, now time.Time) {
	s.act.mu.Lock()
	if now.Sub(s.act.lastPrune) < pruneEvery {
		s.act.mu.Unlock()
		return
	}
	s.act.lastPrune = now
	s.act.mu.Unlock()
	cutoff := now.AddDate(0, 0, -activityKeepDays)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM agent_activity_people WHERE day < $1`, []any{cutoff.Format("2006-01-02")}},
		{`DELETE FROM agent_activity_events WHERE at < $1`, []any{cutoff}},
		{`DELETE FROM agent_activity_minutes WHERE minute < $1`, []any{now.Add(-24 * time.Hour)}},
		{`DELETE FROM agent_activity_events e USING (
			SELECT id, ROW_NUMBER() OVER (PARTITION BY agent_id ORDER BY id DESC) AS n FROM agent_activity_events
		) r WHERE e.id = r.id AND r.n > $1`, []any{activityMaxEvents}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			log.Printf("activity: tidy up: %v", err)
		}
	}
}

// recordJoined notes, for every agent that uses the bot, that someone connected on it.
func (s *Store) recordJoined(ctx context.Context, identityID string) {
	var service string
	if err := s.pool.QueryRow(ctx, `SELECT i.service FROM identities idn JOIN integrations i ON i.id = idn.integration_id WHERE idn.id = $1`, identityID).Scan(&service); err != nil {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT agent_id FROM agent_identities WHERE identity_id = $1`, identityID)
	if err != nil {
		return
	}
	var agents []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			agents = append(agents, a)
		}
	}
	rows.Close()
	for _, a := range agents {
		s.ActivityEvent(ctx, a, domain.ActivityJoined, service)
	}
}

// ActivitySummary implements domain.ActivityReader.
func (s *Store) ActivitySummary(ctx context.Context, agentIDs []string, days int, now time.Time) (domain.ActivitySummary, error) {
	if days < 1 {
		days = 1
	}
	if days > 30 {
		days = 30
	}
	out := domain.ActivitySummary{RecentWindowMinutes: int(domain.RecentMessagesWindow / time.Minute), Daily: []domain.ActivityDay{}, ChatApps: []domain.ActivityCount{}, Topics: []domain.ActivityCount{}, Problems: []domain.ActivityProblem{}, Events: []domain.ActivityEvent{}}
	now = now.UTC()
	today := now.Format("2006-01-02")
	first := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < days; i++ {
		out.Daily = append(out.Daily, domain.ActivityDay{Day: now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")})
	}
	if len(agentIDs) == 0 {
		return out, nil
	}

	one := func(sql string, dest any, args ...any) error { return s.pool.QueryRow(ctx, sql, args...).Scan(dest) }
	if err := one(`SELECT COUNT(DISTINCT mc.user_id) FROM messenger_channels mc JOIN agent_identities ai ON ai.identity_id = mc.identity_id WHERE ai.agent_id = ANY($1)`, &out.Connected, agentIDs); err != nil {
		return out, err
	}
	// New this week: a person whose FIRST connection to these agents is this week's (people who
	// connected before this was tracked have no date and are never counted as new).
	if err := one(`SELECT COUNT(*) FROM (
			SELECT mc.user_id FROM messenger_channels mc JOIN agent_identities ai ON ai.identity_id = mc.identity_id
			WHERE ai.agent_id = ANY($1) GROUP BY mc.user_id
			HAVING BOOL_AND(mc.created_at IS NOT NULL) AND MIN(mc.created_at) >= $2) fresh`, &out.ConnectedThisWeek, agentIDs, weekAgo); err != nil {
		return out, err
	}
	if err := one(`SELECT COUNT(DISTINCT person) FROM agent_activity_people WHERE agent_id = ANY($1) AND day = $2`, &out.ActiveToday, agentIDs, today); err != nil {
		return out, err
	}
	if err := one(`SELECT COUNT(DISTINCT person) FROM agent_activity_people WHERE agent_id = ANY($1) AND day > $2::date - 7`, &out.ActiveWeek, agentIDs, today); err != nil {
		return out, err
	}
	if err := one(`SELECT COUNT(*) FROM agent_activity_events WHERE agent_id = ANY($1) AND kind = 'failed' AND at >= $2`, &out.FailedToday, agentIDs, dayStart); err != nil {
		return out, err
	}

	// People active each day.
	rows, err := s.pool.Query(ctx, `SELECT to_char(day, 'YYYY-MM-DD'), COUNT(DISTINCT person) FROM agent_activity_people WHERE agent_id = ANY($1) AND day >= $2 AND day <= $3 GROUP BY day`, agentIDs, first, today)
	if err != nil {
		return out, err
	}
	byDay := map[string]int{}
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			rows.Close()
			return out, err
		}
		byDay[d] = n
	}
	rows.Close()
	for i := range out.Daily {
		out.Daily[i].People = byDay[out.Daily[i].Day]
	}

	// Where people chat.
	rows, err = s.pool.Query(ctx, `
		SELECT i.service, COUNT(DISTINCT mc.user_id) AS n
		FROM messenger_channels mc
		JOIN agent_identities ai ON ai.identity_id = mc.identity_id
		JOIN identities idn ON idn.id = mc.identity_id
		JOIN integrations i ON i.id = idn.integration_id
		WHERE ai.agent_id = ANY($1) GROUP BY i.service ORDER BY n DESC, i.service`, agentIDs)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c domain.ActivityCount
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			rows.Close()
			return out, err
		}
		out.ChatApps = append(out.ChatApps, c)
	}
	rows.Close()

	// What was asked for and isn't possible: only topics asked for several times.
	rows, err = s.pool.Query(ctx, `
		SELECT detail, COUNT(*) AS n FROM agent_activity_events
		WHERE agent_id = ANY($1) AND kind = 'unsupported' AND detail <> '' AND at >= $2
		GROUP BY detail HAVING COUNT(*) >= $3 ORDER BY n DESC, detail LIMIT 5`, agentIDs, now.AddDate(0, 0, -days), domain.MinTopicCount)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c domain.ActivityCount
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			rows.Close()
			return out, err
		}
		out.Topics = append(out.Topics, c)
	}
	rows.Close()

	// What went wrong in the last day.
	rows, err = s.pool.Query(ctx, `
		SELECT agent_id, kind, detail, COUNT(*), MAX(at) FROM agent_activity_events
		WHERE agent_id = ANY($1) AND kind IN ('failed', 'signin') AND at >= $2
		GROUP BY agent_id, kind, detail ORDER BY MAX(at) DESC LIMIT 10`, agentIDs, now.Add(-24*time.Hour))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var p domain.ActivityProblem
		var kind string
		if err := rows.Scan(&p.AgentID, &kind, &p.Detail, &p.Count, &p.Last); err != nil {
			rows.Close()
			return out, err
		}
		p.Kind = domain.ActivityKind(kind)
		out.Problems = append(out.Problems, p)
	}
	rows.Close()

	// Messages handled lately: one number for the feed's single "messages" line.
	out.RecentWindowMinutes = int(domain.RecentMessagesWindow / time.Minute)
	if err := one(`SELECT COALESCE(SUM(messages), 0) FROM agent_activity_minutes WHERE agent_id = ANY($1) AND minute > $2`, &out.RecentMessages, agentIDs, now.Add(-domain.RecentMessagesWindow)); err != nil {
		return out, err
	}

	// The latest things that happened.
	rows, err = s.pool.Query(ctx, `SELECT id, agent_id, kind, detail, at FROM agent_activity_events WHERE agent_id = ANY($1) ORDER BY id DESC LIMIT 30`, agentIDs)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var e domain.ActivityEvent
		var kind string
		if err := rows.Scan(&e.ID, &e.AgentID, &kind, &e.Detail, &e.At); err != nil {
			rows.Close()
			return out, err
		}
		e.Kind = domain.ActivityKind(kind)
		out.Events = append(out.Events, e)
	}
	rows.Close()
	return out, rows.Err()
}
