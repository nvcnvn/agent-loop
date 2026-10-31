package model

import (
	"context"
	"errors"
	"fmt"
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
// errors, timeouts and dropped connections, but not cancellation.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	_, isNet := errors.AsType[net.Error](err)
	return isNet || errors.Is(err, net.ErrClosed)
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
	return retry(ctx, r, func() (ChatResponse, error) { return r.Provider.Chat(ctx, req) })
}

func retry[T any](ctx context.Context, r *Retry, call func() (T, error)) (T, error) {
	backoff := r.Base
	for attempt := 1; ; attempt++ {
		out, err := call()
		if err == nil || attempt >= r.Attempts || ctx.Err() != nil || !Transient(err) {
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
