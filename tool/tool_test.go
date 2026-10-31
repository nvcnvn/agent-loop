package tool_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
)

var noop = tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) { return tool.Result{}, nil })

func TestRegisterValidatesAndFillsDefaults(t *testing.T) {
	r := tool.NewRegistry()
	if err := r.Register(core.ToolDefinition{}, noop); err == nil {
		t.Fatal("missing name accepted")
	}
	if err := r.Register(core.ToolDefinition{Name: "x"}, nil); err == nil {
		t.Fatal("missing executor accepted")
	}
	if err := r.Register(core.ToolDefinition{Name: "x"}, noop); err != nil {
		t.Fatal(err)
	}
	def, exec, ok := r.Get("x")
	if !ok || exec == nil {
		t.Fatal("registered tool not found")
	}
	if def.ID == "" || def.Version != "1.0.0" || def.Idempotency != core.IdemUnknown ||
		def.SideEffect != core.SideNone || def.Concurrency != core.ConcurrencySerial {
		t.Fatalf("defaults not applied: %+v", def)
	}
	if _, _, ok := r.Get("missing"); ok {
		t.Fatal("unknown tool found")
	}
}

func TestBuiltins(t *testing.T) {
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r); err != nil {
		t.Fatal(err)
	}
	if n := len(r.List()); n != 3 {
		t.Fatalf("builtins=%d", n)
	}
	cases := []struct{ name, in, want string }{
		{"echo", `{"text":"hi"}`, `{"text":"hi"}`},
		{"uppercase", `{"text":"hi"}`, `{"text":"HI"}`},
		{"math.add", `{"a":19,"b":23}`, `{"sum":42}`},
	}
	for _, tc := range cases {
		_, exec, _ := r.Get(tc.name)
		res, err := exec.Execute(context.Background(), tool.Call{Input: json.RawMessage(tc.in)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(res.Output) != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, res.Output, tc.want)
		}
	}
}

func FuzzBuiltinArguments(f *testing.F) {
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r); err != nil {
		f.Fatal(err)
	}
	defs := r.List()
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	f.Add(uint8(0), []byte(`{"text":"hi"}`))
	f.Add(uint8(1), []byte(`{"a":1e308,"b":1e308}`))
	f.Add(uint8(2), []byte(`{"text":123}`))
	f.Add(uint8(0), []byte(`null`))
	f.Fuzz(func(t *testing.T, which uint8, input []byte) {
		def := defs[int(which)%len(defs)]
		_, exec, _ := r.Get(def.Name)
		res, err := exec.Execute(context.Background(), tool.Call{Tool: def, Input: input})
		if err == nil && !json.Valid(res.Output) {
			t.Fatalf("%s returned invalid JSON %q", def.Name, res.Output)
		}
	})
}
