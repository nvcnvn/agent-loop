package runtime

import (
	"context"
	"errors"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Replay rebuilds the terminal state of a run by re-reading its events.
//
// This is the "strict replay" mode described in docs/04-contracts-and-tests.md:
// we do not re-call the model or tools; we recompute projections from the
// stored event stream and verify the run reaches its persisted terminal state.
func Replay(ctx context.Context, s store.Store, tenant core.TenantID, runID core.RunID) (Result, error) {
	events, err := s.ListRunEvents(ctx, tenant, runID, 0, 0)
	if err != nil {
		return Result{}, err
	}
	if len(events) == 0 {
		return Result{}, core.ErrNotFound
	}

	r, err := s.GetRun(ctx, tenant, runID)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		RunID:   runID,
		Status:  core.RunQueued,
		Outputs: map[string]string{},
	}
	var lastSeq int64
	for _, ev := range events {
		if ev.Sequence <= lastSeq {
			return Result{}, errors.New("replay: non-monotonic sequence in stored events")
		}
		lastSeq = ev.Sequence
		if ev.Type == core.EvRunStateChanged {
			var p struct {
				Status core.RunStatus `json:"status"`
			}
			_ = jsonUnmarshal(ev.Payload, &p)
			if p.Status != "" {
				res.Status = p.Status
			}
		}
	}

	if res.Status != r.Status {
		return res, errors.New("replay: terminal status mismatch with stored run")
	}
	res.Usage = r.Usage
	res.FailureReason = r.FailureReason
	return res, nil
}

func jsonUnmarshal(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	return jsonUnmarshalImpl(b, v)
}
