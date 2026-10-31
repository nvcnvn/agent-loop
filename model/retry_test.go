package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

type flaky struct {
	MockProvider
	errs  []error
	calls int
}

func (f *flaky) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return ChatResponse{}, err
	}
	return ChatResponse{Message: Message{Content: "ok"}}, nil
}

func TestRetryRetriesOnlyTransientErrors(t *testing.T) {
	busy := &StatusError{Code: 503}
	cases := []struct {
		name      string
		errs      []error
		wantCalls int
		wantErr   bool
	}{
		{"recovers after transient errors", []error{busy, &StatusError{Code: 429}}, 3, false},
		{"gives up after Attempts", []error{busy, busy, busy, busy}, 3, true},
		{"does not retry a client error", []error{&StatusError{Code: 400}}, 1, true},
		{"does not retry cancellation", []error{context.Canceled}, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &flaky{errs: c.errs}
			r := &Retry{Provider: f, Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}
			resp, err := r.Chat(context.Background(), ChatRequest{})
			if f.calls != c.wantCalls || (err != nil) != c.wantErr {
				t.Fatalf("calls=%d err=%v", f.calls, err)
			}
			if !c.wantErr && resp.Message.Content != "ok" {
				t.Fatalf("resp=%+v", resp)
			}
		})
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &flaky{errs: []error{&StatusError{Code: 503, RetryAfter: time.Hour}}}
	r := &Retry{Provider: f, Attempts: 5, Base: time.Millisecond, Max: time.Hour}
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := r.Chat(ctx, ChatRequest{})
	var se *StatusError
	if f.calls != 1 || !errors.As(err, &se) {
		t.Fatalf("calls=%d err=%v", f.calls, err)
	}
}
