package policy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/policy"
)

func TestSemaphoresBoundEachKey(t *testing.T) {
	var s policy.Semaphores
	ctx := context.Background()
	release, err := s.Acquire(ctx, "tool:a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := s.Acquire(ctx, "tool:b", 1); err != nil {
		t.Fatalf("other key blocked: %v", err)
	} else {
		other()
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(short, "tool:a", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second holder err=%v want DeadlineExceeded", err)
	}
	release()
	again, err := s.Acquire(ctx, "tool:a", 1)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
	if unlimited, err := s.Acquire(short, "any", 0); err != nil {
		t.Fatalf("limit 0 must not block: %v", err)
	} else {
		unlimited()
	}
	if n := s.Keys(); n != 0 {
		t.Fatalf("%d idle keys kept", n)
	}
}
