package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Config holds the agents, named workflow templates and history compaction an
// operator defines in a file.
type Config struct {
	Agents     []Agent     `json:"agents"`
	Workflows  []Workflow  `json:"workflows,omitempty"`
	Compaction *Compaction `json:"compaction,omitempty"`
}

// ParseConfig decodes a JSON config and checks its agents and workflows against each other.
func ParseConfig(data []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate checks the agents and workflows against each other.
func (c Config) Validate() error {
	agents := map[string]Agent{}
	for _, a := range c.Agents {
		if a.Name == "" {
			return errors.New("agent without a name")
		}
		if _, dup := agents[a.Name]; dup {
			return fmt.Errorf("duplicate agent %q", a.Name)
		}
		if a.Timeout < 0 || a.MaxToolLoops < 0 {
			return fmt.Errorf("agent %q: timeout and max_tool_loops must not be negative", a.Name)
		}
		agents[a.Name] = a
	}
	for _, a := range c.Agents {
		for _, child := range a.Children {
			if _, ok := agents[child]; !ok {
				return fmt.Errorf("agent %q: unknown child agent %q", a.Name, child)
			}
		}
	}
	if c := c.Compaction; c != nil {
		if _, ok := agents[c.Agent]; !ok {
			return fmt.Errorf("compaction: unknown agent %q", c.Agent)
		}
		if c.MaxMessages < 0 || c.MaxTokens < 0 || c.KeepRecent < 0 || c.MaxMessages == 0 && c.MaxTokens == 0 {
			return errors.New("compaction: needs max_messages or max_tokens, and no negative values")
		}
	}
	names := map[string]bool{}
	for _, w := range c.Workflows {
		if w.Name == "" || names[w.Name] {
			return fmt.Errorf("workflow name %q is empty or used twice", w.Name)
		}
		names[w.Name] = true
		if _, err := compileWorkflow(w, agents); err != nil {
			return fmt.Errorf("workflow %q: %w", w.Name, err)
		}
	}
	return nil
}
