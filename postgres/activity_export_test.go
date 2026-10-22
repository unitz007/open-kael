package postgres

import (
	"context"
	"time"
)

// Helpers for the activity tests only (this file is compiled for tests and nothing else).

func (s *Store) PruneActivityForTest(ctx context.Context) error {
	s.act.mu.Lock()
	s.act.lastPrune = time.Time{}
	s.act.mu.Unlock()
	s.pruneActivity(ctx, time.Now().UTC())
	return nil
}

func (s *Store) RawActivityPeopleForTest(ctx context.Context) []string {
	rows, err := s.pool.Query(ctx, `SELECT person FROM agent_activity_people`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// SetActivityDaysForTest writes people for given days as they would be stored.
func (s *Store) SetActivityDaysForTest(ctx context.Context, agentID string, days map[string][]string) {
	salt, err := s.activitySalt(ctx)
	if err != nil {
		panic(err)
	}
	for day, people := range days {
		for _, p := range people {
			if _, err := s.pool.Exec(ctx, `INSERT INTO agent_activity_people (agent_id, day, person) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, agentID, day, personCode(salt, p)); err != nil {
				panic(err)
			}
		}
	}
}

func (s *Store) BackdateChannelsForTest(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE messenger_channels SET created_at = NOW() - INTERVAL '30 days' WHERE id = $1`, id)
	return err
}

func (s *Store) AgeEventsForTest(ctx context.Context, agentID string, days int) error {
	_, err := s.pool.Exec(ctx, `UPDATE agent_activity_events SET at = NOW() - make_interval(days => $2) WHERE agent_id = $1`, agentID, days)
	return err
}

// AddOldMessagesForTest records n messages as if they were handled a while ago.
func (s *Store) AddOldMessagesForTest(ctx context.Context, agentID string, ago time.Duration, n int) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO agent_activity_minutes (agent_id, minute, messages) VALUES ($1, $2, $3) ON CONFLICT (agent_id, minute) DO UPDATE SET messages = EXCLUDED.messages`,
		agentID, time.Now().UTC().Add(-ago).Truncate(time.Minute), n)
	return err
}

func (s *Store) MinuteRowsForTest(ctx context.Context, agentID string) int {
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_activity_minutes WHERE agent_id = $1`, agentID).Scan(&n)
	return n
}

// AgeUsesForTest moves an agent's counted uses the given number of days back.
func (s *Store) AgeUsesForTest(ctx context.Context, agentID string, days int) error {
	_, err := s.pool.Exec(ctx, `UPDATE agent_activity_uses SET day = day - $2::int WHERE agent_id = $1`, agentID, days)
	return err
}

// AgeToolsForTest moves an agent's counted ability uses the given number of days back.
func (s *Store) AgeToolsForTest(ctx context.Context, agentID string, days int) error {
	_, err := s.pool.Exec(ctx, `UPDATE agent_activity_tools SET day = day - $2::int WHERE agent_id = $1`, agentID, days)
	return err
}
