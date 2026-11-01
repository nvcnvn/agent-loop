package observe_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/observe"
)

func TestRedactor(t *testing.T) {
	r, err := observe.NewRedactor(observe.DefaultRedactPatterns...)
	if err != nil {
		t.Fatal(err)
	}
	r.Add("hunter2-secret")
	r.Add("abc")
	for in, want := range map[string]string{
		"token is hunter2-secret!":                         "token is [REDACTED]!",
		"Authorization: Bearer eyJhbGciOi.x-y_z":           "Authorization: [REDACTED]",
		"dial postgres://app:pw@db:5432/x":                 "dial [REDACTED]db:5432/x",
		"GET /x?api_key=k123&y=1":                          "GET /x?[REDACTED]&y=1",
		"key sk-abcdefghijklmnopqrstuvwxyz used":           "key [REDACTED] used",
		"abc is too short to hide, and so is a plain word": "abc is too short to hide, and so is a plain word",
		"the bearer of bad news":                           "the bearer of bad news",
	} {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	if (*observe.Redactor)(nil).Redact("hunter2-secret") != "hunter2-secret" {
		t.Fatal("a nil redactor changed text")
	}
	if err := r.AddPattern("("); err == nil {
		t.Fatal("accepted an invalid pattern")
	}

	var buf bytes.Buffer
	log := slog.New(r.Handler(slog.NewJSONHandler(&buf, nil))).With("dsn", "postgres://app:pw@db/x")
	log.Info("leaked hunter2-secret", "err", errors.New("auth hunter2-secret"), slog.Group("req", "header", "Bearer abcdef123"), "n", 3, "tools", []string{"a", "b"})
	if out := buf.String(); strings.Contains(out, "hunter2-secret") || strings.Contains(out, "abcdef123") || strings.Contains(out, "pw@") ||
		!strings.Contains(out, `"n":3`) || !strings.Contains(out, `"tools":["a","b"]`) {
		t.Fatalf("log: %s", out)
	}
}
