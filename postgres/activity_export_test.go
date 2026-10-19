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
