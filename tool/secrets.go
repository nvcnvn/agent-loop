package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/nvcnvn/agent-loop/core"
)

// Secrets gives tools the credentials they need, by name, for the tenant of
// a call, so credentials stay out of tool code, config files and events.
type Secrets interface {
	Secret(ctx context.Context, tenant core.TenantID, name string) (string, error)
}

// ErrNoSecret is returned for a secret a Secrets does not have.
var ErrNoSecret = errors.New("no such secret")

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// DirSecrets reads a secret from the file <Dir>/<tenant>/<name>, or else
// <Dir>/<name>, as a mounted secret volume holds them, trimming trailing line
// breaks. Nothing outside Dir is read.
type DirSecrets struct{ Dir string }

func (d DirSecrets) Secret(_ context.Context, tenant core.TenantID, name string) (string, error) {
	if !secretName.MatchString(name) {
		return "", fmt.Errorf("%w: invalid name %q", ErrNoSecret, name)
	}
	root, err := os.OpenRoot(d.Dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if secretName.MatchString(string(tenant)) {
		if data, err := root.ReadFile(path.Join(string(tenant), name)); err == nil {
			return strings.TrimRight(string(data), "\r\n"), nil
		}
	}
	data, err := root.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// EnvSecrets reads a secret from the environment variable Prefix followed by
// the name upper-cased, with every other character than a letter or digit as "_".
type EnvSecrets struct{ Prefix string }

func (e EnvSecrets) Secret(_ context.Context, _ core.TenantID, name string) (string, error) {
	if !secretName.MatchString(name) {
		return "", fmt.Errorf("%w: invalid name %q", ErrNoSecret, name)
	}
	key := e.Prefix + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name)
	if v, ok := os.LookupEnv(key); ok {
		return v, nil
	}
	return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
}
