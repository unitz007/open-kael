package agent

import (
	"context"
	"time"
)

// TriggerState persists the last time a cron-triggered workflow actually
// fired, so a process that was offline across one or more scheduled
// occurrences can detect that at its next startup and catch up — gocron
// itself has no memory of this (see warnIfCronRunLikelyMissed's own doc
// comment): it only ever schedules the *next* occurrence forward from
// whenever the process starts. Ships no implementation here — same stance
// every other extension point in this package takes (AgentLoop,
// DelegateTarget, memory.Memory, identity.Identity): kael-platform defines
// the contract, the consuming app supplies a durable store.
type TriggerState interface {
	// LastFired returns the last time workflowID successfully fired, and
	// false if nothing has ever been recorded for it (a brand new workflow,
	// or a store that's never seen this id) — distinct from a genuine zero
	// time.Time, so callers don't need a sentinel value to tell them apart.
	LastFired(ctx context.Context, workflowID string) (t time.Time, found bool, err error)
	// RecordFired marks workflowID as having successfully fired at at.
	RecordFired(ctx context.Context, workflowID string, at time.Time) error
}
