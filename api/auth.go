package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
)

// Identity is the tenant, principal and role a request acts as.
type Identity struct {
	Tenant    core.TenantID
	Principal core.PrincipalID
	Role      policy.Role
}

// Authenticator resolves the caller of a request to a non-public route; an error answers 401.
type Authenticator func(*http.Request) (Identity, error)

// HeaderIdentity trusts X-Tenant-ID, X-Principal-ID and X-Role as sent, if
// the IDs are UUIDs and the role a known one (admin when absent); it is for
// local development only.
func HeaderIdentity(r *http.Request) (Identity, error) {
	id := Identity{Tenant: core.DefaultTenant, Principal: core.DefaultPrincipal, Role: policy.RoleAdmin}
	if t := r.Header.Get("X-Tenant-ID"); t != "" {
		id.Tenant = core.TenantID(t)
	}
	if p := r.Header.Get("X-Principal-ID"); p != "" {
		id.Principal = core.PrincipalID(p)
	}
	if role := r.Header.Get("X-Role"); role != "" {
		id.Role = policy.Role(role)
	}
	return id, id.validate()
}

// validate checks the IDs are UUIDs, which the Postgres store requires, and the role is known.
func (id Identity) validate() error {
	if uuid.Validate(string(id.Tenant)) != nil || uuid.Validate(string(id.Principal)) != nil {
		return errors.New("tenant and principal IDs must be UUIDs")
	}
	if !id.Role.Valid() {
		return errors.New("role must be admin, operator, member or viewer")
	}
	return nil
}

var errUnauthenticated = errors.New("missing or unknown API key")

// APIKeys authenticates "Authorization: Bearer <key>" against keys.
func APIKeys(keys map[string]Identity) Authenticator {
	hashed := make(map[[sha256.Size]byte]Identity, len(keys))
	for k, id := range keys {
		if id.Role == "" {
			id.Role = policy.RoleMember
		}
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

// ParseAPIKeys parses comma-separated "[<role>:]<tenant>:<principal>:<key>" entries, member by default, never quoting a key in errors.
func ParseAPIKeys(spec string) (map[string]Identity, error) {
	keys := map[string]Identity{}
	for i, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		role := policy.RoleMember
		if r, rest, ok := strings.Cut(entry, ":"); ok && policy.Role(r).Valid() {
			role, entry = policy.Role(r), rest
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, fmt.Errorf("API key entry %d: want [<role>:]<tenant>:<principal>:<key>", i+1)
		}
		if _, dup := keys[parts[2]]; dup {
			return nil, fmt.Errorf("API key for %s:%s is listed twice", parts[0], parts[1])
		}
		id := Identity{Tenant: core.TenantID(parts[0]), Principal: core.PrincipalID(parts[1]), Role: role}
		if err := id.validate(); err != nil {
			return nil, fmt.Errorf("API key entry %d: %w", i+1, err)
		}
		keys[parts[2]] = id
	}
	return keys, nil
}

const streamRoute = "GET /v1/runs/{rid}/stream"

type identityKey struct{}

func identityOf(r *http.Request) Identity {
	if id, ok := r.Context().Value(identityKey{}).(Identity); ok {
		return id
	}
	return Identity{Tenant: core.DefaultTenant, Principal: core.DefaultPrincipal, Role: policy.RoleAdmin}
}

func withIdentity(r *http.Request, id Identity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
}
