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
		def.SideEffect != core.SideDangerous || def.Concurrency != core.ConcurrencySerial {
		t.Fatalf("defaults not applied: %+v", def)
	}
	if _, _, ok := r.Get("missing"); ok {
		t.Fatal("unknown tool found")
	}
}

func TestRegisterCompilesInputSchemaAndCheckInputUsesIt(t *testing.T) {
	r := tool.NewRegistry()
	if err := r.Register(core.ToolDefinition{Name: "bad", InputSchema: json.RawMessage(`{"type":"nope"}`)}, noop); err == nil {
		t.Fatal("invalid schema accepted")
	}
	if err := r.Register(core.ToolDefinition{Name: "remote", InputSchema: json.RawMessage(`{"$ref":"https://example.com/s.json"}`)}, noop); err == nil {
		t.Fatal("schema referencing another document accepted")
	}
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	if err := r.Register(core.ToolDefinition{Name: "x", InputSchema: schema}, noop); err != nil {
		t.Fatal(err)
	}
	for in, ok := range map[string]bool{`{"n":1}`: true, `{"n":"1"}`: false, `{}`: false, `not json`: false} {
		if err := r.CheckInput("x", json.RawMessage(in)); (err == nil) != ok {
			t.Errorf("CheckInput(%s) = %v", in, err)
		}
	}
	if err := r.CheckInput("missing", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool passed")
	}
}

func TestBuiltins(t *testing.T) {
	r := tool.NewRegistry()
	if err := tool.RegisterBuiltins(r); err != nil {
		t.Fatal(err)
	}
	if n := len(r.List()); n != 4 {
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

func FuzzCheckInput(f *testing.F) {
	r := tool.NewRegistry()
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"s":{"type":"string","maxLength":4}},"required":["n"]}`)
	if err := r.Register(core.ToolDefinition{Name: "x", InputSchema: schema}, noop); err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{`{"n":1}`, `{"n":1.5}`, `{"s":"toolong"}`, `[]`, `"str"`, ``, `{"n":1e999}`, `{"n":` + "\x00"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		_ = r.CheckInput("x", in)
	})
}
