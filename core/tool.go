package core

import "encoding/json"

// IdempotencyHint matches the values in the schema and domain model.
type IdempotencyHint string

const (
	IdemUnknown               IdempotencyHint = "unknown"
	IdemIdempotent            IdempotencyHint = "idempotent"
	IdemConditionalIdempotent IdempotencyHint = "conditionally_idempotent"
	IdemNonIdempotent         IdempotencyHint = "non_idempotent"
)

// SideEffectLevel matches the values in the schema and domain model.
type SideEffectLevel string

const (
	SideNone          SideEffectLevel = "none"
	SideReadExternal  SideEffectLevel = "read_external"
	SideWriteInternal SideEffectLevel = "write_internal"
	SideWriteExternal SideEffectLevel = "write_external"
	SideDangerous     SideEffectLevel = "dangerous"
)

// ConcurrencyClass matches the values in the schema and domain model.
type ConcurrencyClass string

const (
	ConcurrencySerial                ConcurrencyClass = "serial"
	ConcurrencyParallelRead          ConcurrencyClass = "parallel_read"
	ConcurrencyParallelIsolatedWrite ConcurrencyClass = "parallel_isolated_write"
	ConcurrencyExclusive             ConcurrencyClass = "exclusive"
	ConcurrencyExternalSideEffect    ConcurrencyClass = "external_side_effect"
)

// ToolDefinition describes a tool to the model, the policy engine and the runtime.
type ToolDefinition struct {
	ID               ToolID           `json:"id,omitempty"`
	Name             string           `json:"name"`
	Version          string           `json:"version"`
	Description      string           `json:"description"`
	InputSchema      json.RawMessage  `json:"input_schema"`
	OutputSchema     json.RawMessage  `json:"output_schema"`
	Idempotency      IdempotencyHint  `json:"idempotency"`
	SideEffect       SideEffectLevel  `json:"side_effect"`
	Concurrency      ConcurrencyClass `json:"concurrency"`
	RequiresApproval bool             `json:"requires_approval"`
}
