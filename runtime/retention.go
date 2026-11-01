package runtime

import (
	"context"
	"time"
)

// Retention is how long data is kept; zero keeps it for ever.
type Retention struct {
	// Sessions is how long a session in which nothing happens, and no run is
	// unfinished, is kept before it is deleted with everything in it.
	Sessions time.Duration `json:"sessions,omitempty"`
	// Memories is how long a memory is kept after it was saved.
	Memories time.Duration `json:"memories,omitempty"`
}

// Purge deletes what Retention no longer keeps.
func (o *Orchestrator) Purge(ctx context.Context) error {
	if r := o.Retention.Sessions; r > 0 {
		n, err := o.Store.PurgeSessions(ctx, time.Now().Add(-r))
		if err != nil {
			return err
		}
		if n > 0 {
			o.Log.InfoContext(ctx, "purged idle sessions", "count", n, "idle_for", r)
		}
	}
	if r := o.Retention.Memories; r > 0 {
		n, err := o.Store.PurgeMemories(ctx, time.Now().Add(-r))
		if err != nil {
			return err
		}
		if n > 0 {
			o.Log.InfoContext(ctx, "purged old memories", "count", n, "older_than", r)
		}
	}
	return nil
}

// KeepPurging calls Purge now and then every interval until ctx ends.
func (o *Orchestrator) KeepPurging(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := o.Purge(ctx); err != nil {
			o.Log.ErrorContext(ctx, "purge", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
