package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// StatusError is a non-2xx HTTP response from a provider.
type StatusError struct {
	Provider   string
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Provider, e.Code, e.Body)
}

// NewStatusError builds a StatusError from resp, reading Retry-After.
func NewStatusError(provider string, resp *http.Response, body []byte) *StatusError {
	e := &StatusError{Provider: provider, Code: resp.StatusCode, Body: string(body)}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

// Transient reports whether err is worth retrying: rate limits, server
// errors, timeouts, dropped connections and replies cut off, but not cancellation.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	_, isNet := errors.AsType[net.Error](err)
	return isNet || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Retry wraps a Provider so Chat retries transient failures with jittered
// exponential backoff, honouring Retry-After.
type Retry struct {
	Provider
	Attempts int           // total tries; below 1 means 1
	Base     time.Duration // first backoff; doubles per try
	Max      time.Duration // backoff ceiling
}

// WithRetry returns p retried up to 4 times, backing off from 500ms to 20s.
func WithRetry(p Provider) *Retry {
	return &Retry{Provider: p, Attempts: 4, Base: 500 * time.Millisecond, Max: 20 * time.Second}
}

func (r *Retry) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	return retry(ctx, r, func() (ChatResponse, bool, error) {
		resp, err := r.Provider.Chat(ctx, req)
		return resp, true, err
	})
}

// ChatStream streams when the wrapped provider can, and otherwise calls Chat.
// A stream that fails after its first delta is not retried, since the
// caller has already seen part of the reply.
func (r *Retry) ChatStream(ctx context.Context, req ChatRequest, onDelta func(Delta)) (ChatResponse, error) {
	s, ok := r.Provider.(Streamer)
	if !ok {
		return r.Chat(ctx, req)
	}
	return retry(ctx, r, func() (ChatResponse, bool, error) {
		started := false
		resp, err := s.ChatStream(ctx, req, func(d Delta) {
			started = true
			onDelta(d)
		})
		return resp, !started, err
	})
}

// Embed retries the wrapped provider's Embed.
func (r *Retry) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	e, ok := r.Provider.(Embedder)
	if !ok {
		return nil, fmt.Errorf("%s embeddings: %w", r.Name(), ErrUnsupported)
	}
	return retry(ctx, r, func() ([][]float32, bool, error) {
		v, err := e.Embed(ctx, model, texts)
		return v, true, err
	})
}

func retry[T any](ctx context.Context, r *Retry, call func() (T, bool, error)) (T, error) {
	backoff := r.Base
	for attempt := 1; ; attempt++ {
		out, retryable, err := call()
		if err == nil || !retryable || attempt >= r.Attempts || ctx.Err() != nil || !Transient(err) {
			return out, err
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		if se, ok := errors.AsType[*StatusError](err); ok && se.RetryAfter > wait {
			wait = se.RetryAfter
		}
		wait = min(wait, r.Max)
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(wait):
		}
		backoff = min(backoff*2, r.Max)
	}
}
