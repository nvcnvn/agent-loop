package policy

import (
	"context"
	"sync"
)

// ConcurrencyLimits bounds work executing at once; zero means unlimited.
// Workflow keys are per tenant; agent and tool keys are shared by all tenants
// because agents and tools are server resources.
type ConcurrencyLimits struct {
	PerTenant   int // runs per tenant
	PerWorkflow int // runs per tenant and workflow name
	PerAgent    int // model calls in flight per agent name
	PerTool     int // executions per tool name; exclusive tools always run one at a time
}

// Semaphores bounds concurrent holders of each key. A key's limit is fixed
// while anyone holds or waits for it; idle keys are forgotten.
type Semaphores struct {
	mu    sync.Mutex
	slots map[string]*semaphore
}

type semaphore struct {
	ch    chan struct{}
	users int
}

// Acquire blocks until key has fewer than limit holders or ctx ends, and
// returns the func that gives the slot back. A limit of zero or less never blocks.
func (s *Semaphores) Acquire(ctx context.Context, key string, limit int) (release func(), err error) {
	if limit <= 0 {
		return func() {}, nil
	}
	s.mu.Lock()
	if s.slots == nil {
		s.slots = map[string]*semaphore{}
	}
	sem := s.slots[key]
	if sem == nil {
		sem = &semaphore{ch: make(chan struct{}, limit)}
		s.slots[key] = sem
	}
	sem.users++
	s.mu.Unlock()
	select {
	case sem.ch <- struct{}{}:
		return func() {
			<-sem.ch
			s.leave(key, sem)
		}, nil
	case <-ctx.Done():
		s.leave(key, sem)
		return nil, ctx.Err()
	}
}

func (s *Semaphores) leave(key string, sem *semaphore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sem.users--; sem.users == 0 {
		delete(s.slots, key)
	}
}

// Keys returns how many keys are held or waited on.
func (s *Semaphores) Keys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}
