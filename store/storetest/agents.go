package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// Agents checks an AgentStore versions a tenant's definitions and keeps tenants apart.
func Agents(t *testing.T, s store.AgentStore) {
	t.Helper()
	ctx := context.Background()
	other := core.TenantID(core.NewID())
	put := func(tenant core.TenantID, name, def string) int {
		t.Helper()
		v, err := s.PutAgent(ctx, store.AgentDefinition{TenantID: tenant, Name: name, Definition: json.RawMessage(def)})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := put(core.DefaultTenant, "writer", `{"name":"writer","role":"drafts"}`); v != 1 {
		t.Fatalf("first version %d", v)
	}
	if v := put(core.DefaultTenant, "writer", `{"name":"writer","role":"rewrites"}`); v != 2 {
		t.Fatalf("second version %d", v)
	}
	put(core.DefaultTenant, "critic", `{"name":"critic"}`)
	put(other, "writer", `{"name":"writer"}`)
	list, err := s.ListAgents(ctx, core.DefaultTenant)
	if err != nil || len(list) != 2 || list[0].Name != "critic" || list[1].Version != 2 || list[1].UpdatedAt.IsZero() {
		t.Fatalf("agents=%+v err=%v", list, err)
	}
	var def struct{ Role string }
	if json.Unmarshal(list[1].Definition, &def) != nil || def.Role != "rewrites" {
		t.Fatalf("definition %s", list[1].Definition)
	}
	if err := s.DeleteAgent(ctx, core.DefaultTenant, "critic"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, core.DefaultTenant, "critic"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if list, _ := s.ListAgents(ctx, other); len(list) != 1 || list[0].Version != 1 {
		t.Fatalf("other tenant's agents=%+v", list)
	}
}
