package pg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(b), 32)))
}

func TestSealer(t *testing.T) {
	old, err := NewSealer("k1:" + key('a'))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewSealer("k2:" + key('b') + ",k1:" + key('a'))
	if err != nil {
		t.Fatal(err)
	}
	sealed := old.text("messages.content", "the secret plan")
	if !strings.HasPrefix(sealed, sealPrefix+"k1:") || strings.Contains(sealed, "secret") {
		t.Fatalf("sealed text %q", sealed)
	}
	if got, err := rotated.openText("messages.content", sealed); err != nil || got != "the secret plan" {
		t.Fatalf("a rotated sealer read %q, %v", got, err)
	}
	if !strings.HasPrefix(rotated.text("messages.content", "x"), sealPrefix+"k2:") {
		t.Fatal("a rotated sealer does not seal with its first key")
	}
	if _, err := old.openText("memories.content", sealed); err == nil {
		t.Fatal("a value moved to another column opened")
	}
	if _, err := (*Sealer)(nil).openText("messages.content", sealed); !errors.Is(err, ErrSealed) {
		t.Fatalf("no sealer: %v", err)
	}
	if got, _ := old.openText("messages.content", "written before sealing"); got != "written before sealing" {
		t.Fatalf("plain text read as %q", got)
	}
	js := old.json("run_events.payload", []byte(`{"tool":"crm"}`))
	if got, err := old.openJSON("run_events.payload", js); err != nil || string(got) != `{"tool":"crm"}` {
		t.Fatalf("json %s: %q %v", js, got, err)
	}
	if got, _ := old.openJSON("run_events.payload", []byte(`{"plain":true}`)); string(got) != `{"plain":true}` {
		t.Fatalf("plain json read as %s", got)
	}
	if old.json("run_events.hidden_payload", nil) != nil || old.bytes("artifacts.content", nil) != nil {
		t.Fatal("nil was sealed")
	}
	b := old.bytes("artifacts.content", []byte{0, 1, 2})
	if got, err := old.openBytes("artifacts.content", b); err != nil || string(got) != "\x00\x01\x02" {
		t.Fatalf("bytes: %v %v", got, err)
	}
	for _, bad := range []string{"", "k1", "k1:short", "k1:" + key('a') + ",k1:" + key('b'), ":" + key('a')} {
		if _, err := NewSealer(bad); err == nil {
			t.Errorf("NewSealer(%q) accepted", bad)
		}
	}
}

func FuzzSealRoundTrip(f *testing.F) {
	sealer, err := NewSealer("k1:" + key('a'))
	if err != nil {
		f.Fatal(err)
	}
	f.Add("sealed:v1:hello there")
	f.Add("sealed:v0:x")
	f.Add("plain")
	f.Add("")
	f.Fuzz(func(t *testing.T, v string) {
		for _, s := range []*Sealer{nil, sealer} {
			if got, err := s.openText("c", s.text("c", v)); err != nil || got != v {
				t.Fatalf("text %q with sealer %v: %q %v", v, s != nil, got, err)
			}
			if got, err := s.openBytes("c", s.bytes("c", []byte(v))); err != nil || string(got) != v {
				t.Fatalf("bytes %q with sealer %v: %q %v", v, s != nil, got, err)
			}
			raw, _ := json.Marshal(v)
			if got, err := s.openJSON("c", s.json("c", raw)); err != nil || string(got) != string(raw) {
				t.Fatalf("json %s with sealer %v: %s %v", raw, s != nil, got, err)
			}
		}
	})
}
