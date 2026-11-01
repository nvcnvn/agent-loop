package pg

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A stored value starting with sealedMark is sealed:v1:<key id>:<base64 of
// nonce and ciphertext>, or sealed:v0:<plain value> for a plain value that
// itself starts with sealedMark.
const (
	sealedMark  = "sealed:"
	sealPrefix  = "sealed:v1:"
	plainPrefix = "sealed:v0:"
)

// ErrSealed is returned for a sealed value whose key the store lacks.
var ErrSealed = errors.New("value sealed with an unknown key")

var errCorrupt = errors.New("corrupt sealed value")

// Sealer encrypts the sensitive columns of a Store with AES-256-GCM under its first key, opening values sealed under any.
type Sealer struct {
	current string
	keys    map[string]cipher.AEAD
}

// NewSealer parses comma-separated "<id>:<base64 32-byte key>" entries, never quoting a key in errors.
func NewSealer(spec string) (*Sealer, error) {
	s := &Sealer{keys: map[string]cipher.AEAD{}}
	for i, entry := range strings.Split(spec, ",") {
		id, b64, ok := strings.Cut(strings.TrimSpace(entry), ":")
		if !ok || id == "" || strings.ContainsAny(id, ": ") {
			return nil, fmt.Errorf("encryption key entry %d: want <id>:<base64 key>", i+1)
		}
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("encryption key %s: want 32 bytes in base64", id)
		}
		if _, dup := s.keys[id]; dup {
			return nil, fmt.Errorf("encryption key %s is listed twice", id)
		}
		block, _ := aes.NewCipher(key)
		s.keys[id], _ = cipher.NewGCM(block)
		if s.current == "" {
			s.current = id
		}
	}
	return s, nil
}

func (s *Sealer) seal(column string, plain []byte) []byte {
	if s == nil {
		if bytes.HasPrefix(plain, []byte(sealedMark)) {
			return append([]byte(plainPrefix), plain...)
		}
		return plain
	}
	aead := s.keys[s.current]
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	return []byte(sealPrefix + s.current + ":" + base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(column))))
}

func (s *Sealer) open(column string, stored []byte) ([]byte, error) {
	switch {
	case !bytes.HasPrefix(stored, []byte(sealedMark)):
		return stored, nil
	case bytes.HasPrefix(stored, []byte(plainPrefix)):
		return stored[len(plainPrefix):], nil
	}
	id, b64, _ := strings.Cut(strings.TrimPrefix(string(stored), sealPrefix), ":")
	var aead cipher.AEAD
	if s != nil {
		aead = s.keys[id]
	}
	if aead == nil {
		return nil, fmt.Errorf("%w %q in %s", ErrSealed, id, column)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) < aead.NonceSize() {
		return nil, fmt.Errorf("%s: %w", column, errCorrupt)
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(column))
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", column, errCorrupt, err)
	}
	return plain, nil
}

func (s *Sealer) text(column, v string) string {
	if v == "" {
		return v
	}
	return string(s.seal(column, []byte(v)))
}

func (s *Sealer) openText(column, v string) (string, error) {
	plain, err := s.open(column, []byte(v))
	return string(plain), err
}

func (s *Sealer) json(column string, raw []byte) []byte {
	if len(raw) == 0 || s == nil && !bytes.HasPrefix(raw, []byte(`"`+sealedMark)) {
		return raw
	}
	stored := plainPrefix + string(raw)
	if s != nil {
		stored = string(s.seal(column, raw))
	}
	out, _ := json.Marshal(stored)
	return out
}

func (s *Sealer) openJSON(column string, raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, []byte(`"`+sealedMark)) {
		return raw, nil
	}
	var stored string
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(stored, sealedMark) {
		return raw, nil
	}
	return s.open(column, []byte(stored))
}

func (s *Sealer) bytes(column string, b []byte) []byte {
	if b == nil {
		return nil
	}
	return s.seal(column, b)
}

func (s *Sealer) openBytes(column string, b []byte) ([]byte, error) {
	return s.open(column, b)
}
