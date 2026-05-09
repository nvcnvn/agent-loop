package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BuiltinEcho is a trivial parallel-safe tool used in examples and tests.
func BuiltinEcho() (Definition, Executor) {
	def := Definition{
		Name:        "echo",
		Version:     "1.0.0",
		Description: "Echo back the provided text. Read-only, parallel safe.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"text":{"type":"string"}},
			"required":["text"]
		}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Idempotency:  IdemIdempotent,
		SideEffect:   SideNone,
		Concurrency:  ConcurrencyParallelRead,
	}
	exec := ExecutorFunc(func(_ context.Context, c Call) (Result, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return Result{}, fmt.Errorf("echo: %w", err)
		}
		out, _ := json.Marshal(map[string]string{"text": in.Text})
		return Result{Output: out}, nil
	})
	return def, exec
}

// BuiltinUppercase is a deterministic parallel-safe text tool.
func BuiltinUppercase() (Definition, Executor) {
	def := Definition{
		Name:        "uppercase",
		Version:     "1.0.0",
		Description: "Return the uppercase version of the provided text.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"text":{"type":"string"}},
			"required":["text"]
		}`),
		Idempotency: IdemIdempotent,
		SideEffect:  SideNone,
		Concurrency: ConcurrencyParallelRead,
	}
	exec := ExecutorFunc(func(_ context.Context, c Call) (Result, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return Result{}, err
		}
		out, _ := json.Marshal(map[string]string{"text": strings.ToUpper(in.Text)})
		return Result{Output: out}, nil
	})
	return def, exec
}

// BuiltinAdd adds two numbers; demonstrates structured output.
func BuiltinAdd() (Definition, Executor) {
	def := Definition{
		Name:        "math.add",
		Version:     "1.0.0",
		Description: "Return the sum of a and b.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{"a":{"type":"number"},"b":{"type":"number"}},
			"required":["a","b"]
		}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"sum":{"type":"number"}}}`),
		Idempotency:  IdemIdempotent,
		SideEffect:   SideNone,
		Concurrency:  ConcurrencyParallelRead,
	}
	exec := ExecutorFunc(func(_ context.Context, c Call) (Result, error) {
		var in struct {
			A float64 `json:"a"`
			B float64 `json:"b"`
		}
		if err := json.Unmarshal(c.Input, &in); err != nil {
			return Result{}, err
		}
		out, _ := json.Marshal(map[string]float64{"sum": in.A + in.B})
		return Result{Output: out}, nil
	})
	return def, exec
}

// RegisterBuiltins wires the demo tools into a registry.
func RegisterBuiltins(r *Registry) error {
	for _, ctor := range []func() (Definition, Executor){BuiltinEcho, BuiltinUppercase, BuiltinAdd} {
		def, exec := ctor()
		if err := r.Register(def, exec); err != nil {
			return err
		}
	}
	return nil
}
