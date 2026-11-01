package observe

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Redacted replaces what a Redactor hides.
const Redacted = "[REDACTED]"

// minSecret is the shortest value Add hides, so a short one does not blank out common text.
const minSecret = 6

// Redactor hides secrets in logs and trace errors: every value it was given
// and every match of its patterns becomes Redacted. A nil Redactor hides nothing.
type Redactor struct {
	mu       sync.RWMutex
	values   []string
	patterns []*regexp.Regexp
}

// DefaultRedactPatterns match bearer tokens and common API key shapes.
var DefaultRedactPatterns = []string{
	`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`,
	`\bsk-[A-Za-z0-9_-]{16,}`,
	`(?i)\b(?:\w*[_-])?(api[_-]?key|token|secret|password)=[^\s&"']+`,
	`postgres(ql)?://[^:/\s]+:[^@\s]+@`,
}

// NewRedactor compiles patterns, Go regular expressions.
func NewRedactor(patterns ...string) (*Redactor, error) {
	r := &Redactor{}
	for _, p := range patterns {
		if err := r.AddPattern(p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// AddPattern hides every match of the Go regular expression p from now on.
func (r *Redactor) AddPattern(p string) error {
	if r == nil {
		return errors.New("no redactor")
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.patterns = append(r.patterns, re)
	return nil
}

// Add hides secret wherever it appears from now on; values shorter than six bytes are ignored.
func (r *Redactor) Add(secret string) {
	if r == nil || len(secret) < minSecret {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.values, secret) {
		r.values = append(r.values, secret)
		slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
	}
}

// Redact returns s with every secret and pattern match hidden.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	for _, re := range r.patterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// Handler wraps h so the message and every attribute it logs are redacted.
func (r *Redactor) Handler(h slog.Handler) slog.Handler {
	return &redactingHandler{next: h, r: r}
}

type redactingHandler struct {
	next slog.Handler
	r    *Redactor
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, h.r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = h.attr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(redacted), r: h.r}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), r: h.r}
}

func (h *redactingHandler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.r.Redact(v.String()))
	case slog.KindGroup:
		group := v.Group()
		redacted := make([]any, len(group))
		for i, g := range group {
			redacted[i] = h.attr(g)
		}
		return slog.Group(a.Key, redacted...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, h.r.Redact(err.Error()))
		}
		if s := v.String(); h.r.Redact(s) != s {
			return slog.String(a.Key, h.r.Redact(s))
		}
	}
	return a
}
