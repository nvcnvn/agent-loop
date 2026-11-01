package runtime

import (
	"slices"
	"sort"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/tool"
)

// registryFor returns the registry holding a tool a tenant's runs may call,
// the tenant's own before the shared one, and the concurrency slot its calls take.
func (o *Orchestrator) registryFor(tenant core.TenantID, name string) (*tool.Registry, string, bool) {
	if reg := o.TenantTools[tenant]; reg != nil {
		if _, _, ok := reg.Get(name); ok {
			return reg, "tool:" + string(tenant) + "/" + name, true
		}
	}
	if _, _, ok := o.Tools.Get(name); ok {
		return o.Tools, "tool:" + name, true
	}
	return nil, "", false
}

// ListTools lists the tools a tenant's runs may call, its own replacing shared ones of the same name.
func (o *Orchestrator) ListTools(tenant core.TenantID) []core.ToolDefinition {
	var own []core.ToolDefinition
	if reg := o.TenantTools[tenant]; reg != nil {
		own = reg.List()
	}
	out := slices.Clone(own)
	for _, def := range o.Tools.List() {
		if !slices.ContainsFunc(own, func(d core.ToolDefinition) bool { return d.Name == def.Name }) {
			out = append(out, def)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// toolPolicies are the policies a run's tool calls must pass: the
// orchestrator's, its tenant's and its workflow's.
func (o *Orchestrator) toolPolicies(in StartRunInput) []policy.ToolPolicy {
	policies := []policy.ToolPolicy{o.Policy}
	if p, ok := o.TenantPolicies[in.TenantID]; ok {
		policies = append(policies, p)
	}
	if in.Workflow.ToolPolicy != nil {
		policies = append(policies, *in.Workflow.ToolPolicy)
	}
	return policies
}
