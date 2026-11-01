package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/store"
)

// ErrInvalidAgent is returned for an agent definition the API cannot store.
var ErrInvalidAgent = errors.New("invalid agent")

var agentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// AgentInfo is an agent a tenant's runs may use: one of Orchestrator.Agents
// (Source "config"), or one the tenant put over the API (Source "api").
type AgentInfo struct {
	Agent
	Source  string `json:"source"`
	Version int    `json:"version,omitempty"`
}

// agentsFor returns the agents a tenant's runs use: Orchestrator.Agents and
// the tenant's own, which run on Orchestrator.Provider and, unless they name
// one, Orchestrator.Model.
func (o *Orchestrator) agentsFor(ctx context.Context, tenant core.TenantID) (map[string]Agent, error) {
	infos, err := o.ListAgents(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Agent, len(infos))
	for _, info := range infos {
		out[info.Name] = info.Agent
	}
	return out, nil
}

// ListAgents lists the agents a tenant's runs may use, by name.
func (o *Orchestrator) ListAgents(ctx context.Context, tenant core.TenantID) ([]AgentInfo, error) {
	out := make([]AgentInfo, 0, len(o.Agents))
	for _, a := range o.Agents {
		out = append(out, AgentInfo{Agent: a, Source: "config"})
	}
	stored, err := o.Store.ListAgents(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, d := range stored {
		var a Agent
		if err := json.Unmarshal(d.Definition, &a); err != nil {
			o.Log.WarnContext(ctx, "skipping a stored agent that does not decode", "tenant_id", tenant, "agent", d.Name, "err", err)
			continue
		}
		if _, global := o.Agents[d.Name]; global || o.Provider == nil {
			continue
		}
		a.Name, a.tenant = d.Name, tenant
		a.Provider = o.Provider
		if a.Model == "" {
			a.Model = o.Model
		}
		if a.MaxToolLoops == 0 {
			a.MaxToolLoops = defaultToolLoops
		}
		out = append(out, AgentInfo{Agent: a, Source: "api", Version: d.Version})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PutAgent creates or replaces a tenant's agent and returns its version. Its
// name may not be one of Orchestrator.Agents, and its tools and children must exist.
func (o *Orchestrator) PutAgent(ctx context.Context, tenant core.TenantID, a Agent) (int, error) {
	if o.Provider == nil {
		return 0, fmt.Errorf("%w: this server runs no agents defined over the API", ErrInvalidAgent)
	}
	if !agentName.MatchString(a.Name) {
		return 0, fmt.Errorf("%w: a name is 1 to 64 letters, digits, '_', '.' or '-'", ErrInvalidAgent)
	}
	if _, global := o.Agents[a.Name]; global {
		return 0, fmt.Errorf("%w: %s is a configured agent", core.ErrConflict, a.Name)
	}
	if a.Timeout < 0 || a.MaxToolLoops < 0 {
		return 0, fmt.Errorf("%w: timeout and max_tool_loops must not be negative", ErrInvalidAgent)
	}
	for _, name := range a.Tools {
		if _, _, ok := o.Tools.Get(name); !ok {
			return 0, fmt.Errorf("%w: unknown tool %q", ErrInvalidAgent, name)
		}
	}
	agents, err := o.agentsFor(ctx, tenant)
	if err != nil {
		return 0, err
	}
	for _, child := range a.Children {
		if _, ok := agents[child]; !ok && child != a.Name {
			return 0, fmt.Errorf("%w: unknown child agent %q", ErrInvalidAgent, child)
		}
	}
	a.Provider = nil
	def, err := json.Marshal(a)
	if err != nil {
		return 0, err
	}
	return o.Store.PutAgent(ctx, store.AgentDefinition{TenantID: tenant, Name: a.Name, Definition: def})
}

// DeleteAgent removes a tenant's agent.
func (o *Orchestrator) DeleteAgent(ctx context.Context, tenant core.TenantID, name string) error {
	if _, global := o.Agents[name]; global {
		return fmt.Errorf("%w: %s is a configured agent", core.ErrConflict, name)
	}
	return o.Store.DeleteAgent(ctx, tenant, name)
}
