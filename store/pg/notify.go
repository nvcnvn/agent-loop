package pg

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notification channels: Append notifies eventsChannel with the run ID of new
// events, and resolving approvals notifies approvalsChannel with the approval ID.
const (
	eventsChannel    = "agentloop_events"
	approvalsChannel = "agentloop_approvals"
)

// notifier holds one connection, outside the pool, that LISTENs on both
// channels from the first watch until Close, and wakes the watchers of each
// notification's key.
type notifier struct {
	pool    *pgxpool.Pool // only its connection config is used
	start   sync.Once
	stop    context.CancelFunc
	stopped chan struct{}

	mu       sync.Mutex
	watchers map[string]map[chan struct{}]struct{}
}

func newNotifier(pool *pgxpool.Pool) *notifier {
	return &notifier{pool: pool, watchers: map[string]map[chan struct{}]struct{}{}}
}

// watch returns a channel that receives a signal whenever channel is notified
// with payload, and whenever notifications may have been missed.
func (n *notifier) watch(channel, payload string) (<-chan struct{}, func()) {
	n.start.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		n.stop, n.stopped = cancel, make(chan struct{})
		go n.listen(ctx)
	})
	key := channel + ":" + payload
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.watchers[key] == nil {
		n.watchers[key] = map[chan struct{}]struct{}{}
	}
	n.watchers[key][ch] = struct{}{}
	n.mu.Unlock()
	return ch, func() {
		n.mu.Lock()
		delete(n.watchers[key], ch)
		if len(n.watchers[key]) == 0 {
			delete(n.watchers, key)
		}
		n.mu.Unlock()
	}
}

func (n *notifier) wake(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.watchers[key] {
		signal(ch)
	}
}

func (n *notifier) wakeAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, set := range n.watchers {
		for ch := range set {
			signal(ch)
		}
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// listen keeps a LISTEN connection open until ctx ends, reconnecting after a
// second when it fails. Watchers are woken whenever the connection is
// (re)established, since notifications sent while it was down are lost.
func (n *notifier) listen(ctx context.Context) {
	defer close(n.stopped)
	for ctx.Err() == nil {
		if err := n.listenOnce(ctx); ctx.Err() == nil {
			slog.Warn("postgres LISTEN connection lost; retrying in a second", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (n *notifier) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, n.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{eventsChannel}.Sanitize()+"; LISTEN "+pgx.Identifier{approvalsChannel}.Sanitize()); err != nil {
		return err
	}
	n.wakeAll()
	for {
		note, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		n.wake(note.Channel + ":" + note.Payload)
	}
}

func (n *notifier) close() {
	n.start.Do(func() {})
	if n.stop != nil {
		n.stop()
		<-n.stopped
	}
	n.wakeAll()
}
