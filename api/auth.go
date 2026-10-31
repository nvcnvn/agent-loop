package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nvcnvn/agent-loop/core"
)

// Identity is the tenant and principal a request acts as.
type Identity struct {
	Tenant    core.TenantID
	Principal core.PrincipalID
}

// Authenticator resolves the caller of a request to a non-public route; an error answers 401.
type Authenticator func(*http.Request) (Identity, error)

// HeaderIdentity trusts X-Tenant-ID and X-Principal-ID as sent; it is for local development only.
func HeaderIdentity(r *http.Request) (Identity, error) {
	id := Identity{Tenant: core.DefaultTenant, Principal: core.DefaultPrincipal}
	if t := r.Header.Get("X-Tenant-ID"); t != "" {
		id.Tenant = core.TenantID(t)
	}
	if p := r.Header.Get("X-Principal-ID"); p != "" {
		id.Principal = core.PrincipalID(p)
	}
	return id, nil
}

var errUnauthenticated = errors.New("missing or unknown API key")

// APIKeys authenticates "Authorization: Bearer <key>" against keys.
func APIKeys(keys map[string]Identity) Authenticator {
	hashed := make(map[[sha256.Size]byte]Identity, len(keys))
	for k, id := range keys {
		hashed[sha256.Sum256([]byte(k))] = id
	}
	return func(r *http.Request) (Identity, error) {
		scheme, key, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		ok = ok && strings.EqualFold(scheme, "Bearer")
		// EventSource cannot send headers, so the stream route also takes the key as a parameter.
		if !ok && r.Pattern == streamRoute {
			key, ok = r.URL.Query().Get("access_token"), true
		}
		// Comparing hashes keeps lookup time independent of how much of a key matches.
		id, found := hashed[sha256.Sum256([]byte(key))]
		if !ok || key == "" || !found {
			return Identity{}, errUnauthenticated
		}
		return id, nil
	}
}

// ParseAPIKeys parses comma-separated "<tenant>:<principal>:<key>" entries.
// Errors never quote an entry, since it holds a key.
func ParseAPIKeys(spec string) (map[string]Identity, error) {
	keys := map[string]Identity{}
	for i, entry := range strings.Split(spec, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("API key entry %d: want <tenant>:<principal>:<key>", i+1)
		}
		if _, dup := keys[parts[2]]; dup {
			return nil, fmt.Errorf("API key for %s:%s is listed twice", parts[0], parts[1])
		}
		keys[parts[2]] = Identity{Tenant: core.TenantID(parts[0]), Principal: core.PrincipalID(parts[1])}
	}
	return keys, nil
}

const streamRoute = "GET /v1/runs/{rid}/stream"

type identityKey struct{}

func identityOf(r *http.Request) Identity {
	if id, ok := r.Context().Value(identityKey{}).(Identity); ok {
		return id
	}
	return Identity{Tenant: core.DefaultTenant, Principal: core.DefaultPrincipal}
}

func withIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}
