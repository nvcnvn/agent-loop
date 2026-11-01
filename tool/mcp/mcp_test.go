package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
)

func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVE_STDIO") == "1" {
		_ = testServer().Run(context.Background(), &sdk.StdioTransport{})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type sumIn struct {
	A int `json:"a"`
	B int `json:"b"`
}

type sumOut struct {
	Sum int `json:"sum"`
}

func testServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "calc", Version: "1.2.3"}, nil)
	closedWorld := false
	sdk.AddTool(s, &sdk.Tool{Name: "add", Description: "adds", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closedWorld}},
		func(_ context.Context, _ *sdk.CallToolRequest, in sumIn) (*sdk.CallToolResult, sumOut, error) {
			return nil, sumOut{Sum: in.A + in.B}, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "write", Description: "writes"},
		func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "written"}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "fail", Description: "fails"},
		func(_ context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "disk full"}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "hang", Description: "never answers", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, any, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	return s
}

// inMemory connects each new session to a fresh session of server and keeps
// the server sessions so a test can cut them.
type inMemory struct {
	server *sdk.Server
	mu     sync.Mutex
	conns  []*sdk.ServerSession
	broken bool
}

func (m *inMemory) transport() (sdk.Transport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken {
		return nil, errors.New("server unreachable")
	}
	client, server := sdk.NewInMemoryTransports()
	ss, err := m.server.Connect(context.Background(), server, nil)
	if err != nil {
		return nil, err
	}
	m.conns = append(m.conns, ss)
	return client, nil
}

func (m *inMemory) cut() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ss := range m.conns {
		_ = ss.Close()
	}
}

func (m *inMemory) connects() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

func registered(t *testing.T, cfg Server) (*Client, *tool.Registry, *inMemory) {
	t.Helper()
	srv := &inMemory{server: testServer()}
	c := newClient(cfg, srv.transport)
	t.Cleanup(func() { _ = c.Close() })
	reg := tool.NewRegistry()
	if _, err := c.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	return c, reg, srv
}

func call(t *testing.T, reg *tool.Registry, name, input string) (tool.Result, error) {
	t.Helper()
	_, exec, ok := reg.Get(name)
	if !ok {
		t.Fatalf("tool %s not registered", name)
	}
	return exec.Execute(context.Background(), tool.Call{Input: json.RawMessage(input)})
}

// Acceptance: §MCP Integration #1 and #2 – discover tools and translate their schemas.
func TestRegisterTranslatesTools(t *testing.T) {
	_, reg, _ := registered(t, Server{Name: "calc"})
	add, _, ok := reg.Get("calc.add")
	if !ok {
		t.Fatalf("calc.add missing from %v", reg.List())
	}
	if add.Version != "1.2.3" || add.Description != "adds" || add.Timeout != 30*time.Second ||
		add.SideEffect != core.SideNone || add.Concurrency != core.ConcurrencyParallelRead || add.Idempotency != core.IdemIdempotent {
		t.Fatalf("calc.add=%+v", add)
	}
	if !strings.Contains(string(add.InputSchema), `"a"`) || !strings.Contains(string(add.OutputSchema), `"sum"`) {
		t.Fatalf("schemas: in=%s out=%s", add.InputSchema, add.OutputSchema)
	}
	write, _, _ := reg.Get("calc.write")
	if write.SideEffect != core.SideDangerous || write.Concurrency != core.ConcurrencySerial || write.Idempotency != core.IdemUnknown {
		t.Fatalf("an unannotated tool must get the most cautious class: %+v", write)
	}
}

func TestRegisterSkipsToolWithBadSchema(t *testing.T) {
	s := sdk.NewServer(&sdk.Implementation{Name: "odd", Version: "1"}, nil)
	ok := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{}, nil
	}
	s.AddTool(&sdk.Tool{Name: "remote_ref", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"https://example.com/x.json"}}}`)}, ok)
	s.AddTool(&sdk.Tool{Name: "fine", InputSchema: json.RawMessage(`{"type":"object"}`)}, ok)
	srv := &inMemory{server: s}
	c := newClient(Server{Name: "odd"}, srv.transport)
	t.Cleanup(func() { _ = c.Close() })
	names, err := c.Register(context.Background(), tool.NewRegistry())
	if err != nil || len(names) != 1 || names[0] != "odd.fine" {
		t.Fatalf("names=%v err=%v", names, err)
	}
}

func TestCallResults(t *testing.T) {
	_, reg, _ := registered(t, Server{Name: "calc"})
	if res, err := call(t, reg, "calc.add", `{"a":2,"b":3}`); err != nil || string(res.Output) != `{"sum":5}` {
		t.Fatalf("add: output=%s err=%v", res.Output, err)
	}
	if res, err := call(t, reg, "calc.write", `{}`); err != nil || string(res.Output) != `{"content":"written"}` {
		t.Fatalf("write: output=%s err=%v", res.Output, err)
	}
	if _, err := call(t, reg, "calc.fail", `{}`); err == nil || err.Error() != "disk full" {
		t.Fatalf("a tool error must come back as an error: %v", err)
	}
	if _, err := call(t, reg, "calc.add", `{"a":"x"}`); err == nil {
		t.Fatal("invalid arguments accepted")
	}
}

// Acceptance: §MCP Integration #3 – timeout and retry policy.
func TestReconnectsAndRetriesIdempotentCalls(t *testing.T) {
	_, reg, srv := registered(t, Server{Name: "calc"})
	srv.cut()
	if res, err := call(t, reg, "calc.add", `{"a":1,"b":1}`); err != nil || string(res.Output) != `{"sum":2}` {
		t.Fatalf("idempotent call after a lost connection: output=%s err=%v", res.Output, err)
	}
	srv.cut()
	if _, err := call(t, reg, "calc.write", `{}`); err == nil {
		t.Fatal("a non-idempotent call was retried after a lost connection")
	}
	if _, err := call(t, reg, "calc.write", `{}`); err != nil {
		t.Fatalf("the next call did not reconnect: %v", err)
	}
	if n := srv.connects(); n != 3 {
		t.Fatalf("connected %d times, want 3", n)
	}

	_, exec, _ := reg.Get("calc.hang")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := exec.Execute(ctx, tool.Call{Input: json.RawMessage(`{}`)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung call: err=%v want deadline exceeded", err)
	}
}

func TestServerRejectionKeepsConnection(t *testing.T) {
	c, reg, srv := registered(t, Server{Name: "calc", BreakAfter: 1})
	for range 3 {
		if _, err := call(t, reg, "calc.add", `{"a":"not a number"}`); err == nil || errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("err=%v", err)
		}
	}
	if srv.connects() != 1 || c.failures != 0 {
		t.Fatalf("rejected calls reconnected (%d) or counted as failures (%d)", srv.connects(), c.failures)
	}
}

// Acceptance: §MCP Integration #4 – a circuit breaker trips after repeated failures.
func TestCircuitBreaker(t *testing.T) {
	_, reg, srv := registered(t, Server{Name: "calc", BreakAfter: 2, BreakFor: 50 * time.Millisecond, MaxRetries: 1})
	srv.mu.Lock()
	srv.broken = true
	srv.mu.Unlock()
	srv.cut()
	for range 2 {
		if _, err := call(t, reg, "calc.add", `{"a":1,"b":1}`); err == nil || errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("err=%v, want a connection failure", err)
		}
	}
	if _, err := call(t, reg, "calc.add", `{"a":1,"b":1}`); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err=%v, want ErrCircuitOpen", err)
	}
	srv.mu.Lock()
	srv.broken = false
	srv.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if _, err := call(t, reg, "calc.add", `{"a":1,"b":1}`); err != nil {
		t.Fatalf("circuit did not close after break_for: %v", err)
	}
}

func TestStreamableHTTPSendsHeaders(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "s3cret")
	server := testServer()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer httpSrv.Close()

	c, err := New(Server{Name: "calc", URL: httpSrv.URL, Headers: map[string]string{"Authorization": "Bearer ${MCP_TEST_TOKEN}"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reg := tool.NewRegistry()
	if _, err := c.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	if res, err := call(t, reg, "calc.add", `{"a":20,"b":22}`); err != nil || string(res.Output) != `{"sum":42}` {
		t.Fatalf("output=%s err=%v", res.Output, err)
	}
}

func TestStdioServer(t *testing.T) {
	t.Setenv("MCP_TEST_SERVE_STDIO", "1")
	c, err := New(Server{Name: "calc", Command: []string{os.Args[0], "-test.run=^$"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	reg := tool.NewRegistry()
	if _, err := c.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	if res, err := call(t, reg, "calc.add", `{"a":1,"b":2}`); err != nil || string(res.Output) != `{"sum":3}` {
		t.Fatalf("output=%s err=%v", res.Output, err)
	}
}

func TestNewChecksConfig(t *testing.T) {
	for name, cfg := range map[string]Server{
		"no name":        {Command: []string{"x"}},
		"no transport":   {Name: "s"},
		"two transports": {Name: "s", Command: []string{"x"}, URL: "http://x"},
		"unset secret":   {Name: "s", URL: "http://x", Headers: map[string]string{"Authorization": "Bearer ${MCP_TEST_UNSET}"}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func FuzzToolFromServer(f *testing.F) {
	c := newClient(Server{Name: "srv"}, nil)
	f.Add([]byte(`{"name":"add","inputSchema":{"type":"object","properties":{"a":{"type":"integer"}}},"annotations":{"readOnlyHint":true}}`))
	f.Add([]byte(`{"name":"x","inputSchema":{"$ref":"http://example.com/s.json"}}`))
	f.Add([]byte(`{"name":"x","inputSchema":{"type":"object"},"outputSchema":{"type":5}}`))
	f.Add([]byte(`{"name":"","annotations":{"destructiveHint":false,"openWorldHint":false}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var tl sdk.Tool
		if json.Unmarshal(raw, &tl) != nil {
			return
		}
		def, err := c.definition(&tl, "1")
		if err != nil {
			return
		}
		if !strings.HasPrefix(def.Name, "srv.") {
			t.Fatalf("name %q lacks the server prefix", def.Name)
		}
		if a := tl.Annotations; a == nil && def.SideEffect != core.SideDangerous {
			t.Fatalf("unannotated tool classed %s", def.SideEffect)
		} else if a != nil && a.ReadOnlyHint && (def.SideEffect != core.SideNone && def.SideEffect != core.SideReadExternal || def.Concurrency != core.ConcurrencyParallelRead) {
			t.Fatalf("read-only tool classed %s, %s", def.SideEffect, def.Concurrency)
		}
		_, schemaErr := tool.CompileSchema(def.InputSchema)
		regErr := tool.NewRegistry().Register(def, tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) { return tool.Result{}, nil }))
		if (schemaErr == nil) != (regErr == nil) {
			t.Fatalf("schema error %v but register error %v", schemaErr, regErr)
		}
	})
}

func FuzzCallResult(f *testing.F) {
	f.Add([]byte(`{"content":[{"type":"text","text":"hi"}]}`))
	f.Add([]byte(`{"content":[{"type":"image","data":"aGk=","mimeType":"image/png"}],"isError":true}`))
	f.Add([]byte(`{"content":[],"structuredContent":{"sum":3}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var res sdk.CallToolResult
		if json.Unmarshal(raw, &res) != nil {
			return
		}
		out, err := result(&res)
		if err == nil && !json.Valid(out.Output) {
			t.Fatalf("invalid output %q", out.Output)
		}
	})
}

// Acceptance: §MCP Integration #5 – tools a server adds, changes or removes after startup are picked up.
func TestRegistryFollowsServerToolChanges(t *testing.T) {
	_, reg, srv := registered(t, Server{Name: "calc"})
	before, _, _ := reg.Get("calc.add")
	sdk.AddTool(srv.server, &sdk.Tool{Name: "mul", Description: "multiplies"}, func(_ context.Context, _ *sdk.CallToolRequest, in sumIn) (*sdk.CallToolResult, sumOut, error) {
		return nil, sumOut{Sum: in.A * in.B}, nil
	})
	waitFor(t, func() bool { _, _, ok := reg.Get("calc.mul"); return ok })
	if res, err := call(t, reg, "calc.mul", `{"a":6,"b":7}`); err != nil || !strings.Contains(string(res.Output), "42") {
		t.Fatalf("calc.mul: %s %v", res.Output, err)
	}
	sdk.AddTool(srv.server, &sdk.Tool{Name: "add", Description: "adds, now documented"}, func(_ context.Context, _ *sdk.CallToolRequest, in sumIn) (*sdk.CallToolResult, sumOut, error) {
		return nil, sumOut{Sum: in.A + in.B}, nil
	})
	waitFor(t, func() bool { def, _, _ := reg.Get("calc.add"); return def.Description == "adds, now documented" })
	if after, _, _ := reg.Get("calc.add"); after.ID != before.ID {
		t.Fatalf("a changed tool got a new ID: %s, was %s", after.ID, before.ID)
	}
	srv.server.RemoveTools("add")
	waitFor(t, func() bool { _, _, ok := reg.Get("calc.add"); return !ok })
}

func TestToolNamesOfOthersAreLeftAlone(t *testing.T) {
	reg := tool.NewRegistry()
	mine := tool.ExecutorFunc(func(context.Context, tool.Call) (tool.Result, error) {
		return tool.Result{Output: json.RawMessage(`"mine"`)}, nil
	})
	if err := reg.Register(core.ToolDefinition{Name: "calc.add"}, mine); err != nil {
		t.Fatal(err)
	}
	srv := &inMemory{server: testServer()}
	c := newClient(Server{Name: "calc"}, srv.transport)
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	srv.server.RemoveTools("add")
	sdk.AddTool(srv.server, &sdk.Tool{Name: "mul"}, func(context.Context, *sdk.CallToolRequest, sumIn) (*sdk.CallToolResult, sumOut, error) {
		return nil, sumOut{}, nil
	})
	waitFor(t, func() bool { _, _, ok := reg.Get("calc.mul"); return ok })
	if res, err := call(t, reg, "calc.add", `{}`); err != nil || string(res.Output) != `"mine"` {
		t.Fatalf("the other tool was replaced or removed: %s %v", res.Output, err)
	}
}

func TestClosedClientStaysClosed(t *testing.T) {
	c, _, srv := registered(t, Server{Name: "calc"})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	connects := srv.connects()
	c.resync()
	time.Sleep(100 * time.Millisecond)
	if srv.connects() != connects {
		t.Fatal("a resync reconnected a closed client")
	}
}

// Acceptance: §MCP Integration #5 – a reconnect picks up what changed while the connection was down.
func TestReconnectRefreshesTools(t *testing.T) {
	_, reg, srv := registered(t, Server{Name: "calc"})
	srv.cut()
	srv.server.RemoveTools("write")
	if _, err := call(t, reg, "calc.add", `{"a":1,"b":2}`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, ok := reg.Get("calc.write"); return !ok })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not met in time")
}
