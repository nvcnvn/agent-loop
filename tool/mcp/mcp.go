// Package mcp registers the tools of Model Context Protocol servers in a
// tool.Registry, translating their schemas and annotations into
// core.ToolDefinition, and calls them with a per-call timeout, retries of
// idempotent calls after a lost connection, and a circuit breaker.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
)

// Server configures one MCP server.
type Server struct {
	Name    string   `json:"name"`              // its tools are registered as <name>.<tool>
	Command []string `json:"command,omitempty"` // a program and its arguments, spoken to over stdio
	URL     string   `json:"url,omitempty"`     // a streamable HTTP endpoint
	// Headers go with every HTTP request. ${VAR} in a value is read from the
	// environment, so credentials stay out of the config file.
	Headers    map[string]string `json:"headers,omitempty"`
	Timeout    time.Duration     `json:"timeout,omitempty"`     // per call; default 30s
	MaxRetries int               `json:"max_retries,omitempty"` // reconnect-and-retry rounds for idempotent calls; default 2
	BreakAfter int               `json:"break_after,omitempty"` // consecutive connection failures that open the circuit; default 5
	BreakFor   time.Duration     `json:"break_for,omitempty"`   // how long an open circuit refuses calls; default 30s
}

// ErrCircuitOpen is returned while a server that kept failing is left alone.
var ErrCircuitOpen = errors.New("mcp: circuit open")

// Client keeps a session with one MCP server and reconnects when it is lost.
type Client struct {
	cfg       Server
	transport func() (sdk.Transport, error)
	client    *sdk.Client

	mu        sync.Mutex
	session   *sdk.ClientSession
	failures  int
	openUntil time.Time
}

// New checks cfg and returns a client that connects on first use.
func New(cfg Server) (*Client, error) {
	if cfg.Name == "" || (len(cfg.Command) == 0) == (cfg.URL == "") {
		return nil, fmt.Errorf("mcp server %q: needs a name and exactly one of command and url", cfg.Name)
	}
	headers := map[string]string{}
	for k, v := range cfg.Headers {
		var missing []string
		headers[k] = os.Expand(v, func(name string) string {
			val, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			return val
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("mcp server %q: header %s uses unset variables %v", cfg.Name, k, missing)
		}
	}
	transport := func() (sdk.Transport, error) {
		cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
		cmd.Stderr = os.Stderr
		return &sdk.CommandTransport{Command: cmd}, nil
	}
	if cfg.URL != "" {
		httpClient := &http.Client{Transport: headerTransport{headers: headers, base: http.DefaultTransport}}
		transport = func() (sdk.Transport, error) {
			return &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient}, nil
		}
	}
	return newClient(cfg, transport), nil
}

func newClient(cfg Server, transport func() (sdk.Transport, error)) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.BreakAfter <= 0 {
		cfg.BreakAfter = 5
	}
	if cfg.BreakFor <= 0 {
		cfg.BreakFor = 30 * time.Second
	}
	return &Client{
		cfg:       cfg,
		transport: transport,
		client:    sdk.NewClient(&sdk.Implementation{Name: "agent-loop", Version: "1"}, nil),
	}
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.base.RoundTrip(r)
}

// Register discovers the server's tools and registers each in reg, returning
// their names. A tool whose input schema does not compile is skipped with a warning.
func (c *Client) Register(ctx context.Context, reg *tool.Registry) ([]string, error) {
	sess, err := c.connected(ctx)
	if err != nil {
		return nil, err
	}
	version := "1.0.0"
	if info := sess.InitializeResult(); info != nil && info.ServerInfo != nil && info.ServerInfo.Version != "" {
		version = info.ServerInfo.Version
	}
	var names []string
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return names, fmt.Errorf("mcp server %q: list tools: %w", c.cfg.Name, err)
		}
		def, err := c.definition(t, version)
		if err != nil {
			return names, err
		}
		remote := t.Name
		idempotent := def.Idempotency == core.IdemIdempotent
		exec := tool.ExecutorFunc(func(ctx context.Context, call tool.Call) (tool.Result, error) {
			return c.call(ctx, remote, call.Input, idempotent)
		})
		if err := reg.Register(def, exec); err != nil {
			slog.WarnContext(ctx, "skipping MCP tool", "server", c.cfg.Name, "tool", t.Name, "err", err)
			continue
		}
		names = append(names, def.Name)
	}
	return names, nil
}

// definition translates an MCP tool. Annotations are hints from the server;
// missing ones default to the most cautious class, as the MCP spec does.
func (c *Client) definition(t *sdk.Tool, version string) (core.ToolDefinition, error) {
	var schema any = t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	in, err := json.Marshal(schema)
	if err != nil {
		return core.ToolDefinition{}, fmt.Errorf("mcp tool %s: input schema: %w", t.Name, err)
	}
	var out json.RawMessage
	if t.OutputSchema != nil {
		if out, err = json.Marshal(t.OutputSchema); err != nil {
			return core.ToolDefinition{}, fmt.Errorf("mcp tool %s: output schema: %w", t.Name, err)
		}
	}
	a := t.Annotations
	if a == nil {
		a = &sdk.ToolAnnotations{}
	}
	description := t.Description
	if description == "" {
		description = a.Title
	}
	def := core.ToolDefinition{
		Name:         c.cfg.Name + "." + t.Name,
		Version:      version,
		Description:  description,
		InputSchema:  in,
		OutputSchema: out,
		Idempotency:  core.IdemUnknown,
		Concurrency:  core.ConcurrencySerial,
		Timeout:      c.cfg.Timeout,
	}
	openWorld := a.OpenWorldHint == nil || *a.OpenWorldHint
	switch destructive := a.DestructiveHint == nil || *a.DestructiveHint; {
	case a.ReadOnlyHint && openWorld:
		def.SideEffect = core.SideReadExternal
	case a.ReadOnlyHint:
		def.SideEffect = core.SideNone
	case destructive:
		def.SideEffect = core.SideDangerous
	case openWorld:
		def.SideEffect = core.SideWriteExternal
	default:
		def.SideEffect = core.SideWriteInternal
	}
	if a.ReadOnlyHint || a.IdempotentHint {
		def.Idempotency = core.IdemIdempotent
	}
	if a.ReadOnlyHint {
		def.Concurrency = core.ConcurrencyParallelRead
	}
	return def, nil
}

// call invokes a tool. An error the server answered with is returned as is;
// any other error means the connection was lost: it is dropped, counted
// toward the circuit breaker and, for idempotent calls, retried.
func (c *Client) call(ctx context.Context, name string, input json.RawMessage, idempotent bool) (tool.Result, error) {
	c.mu.Lock()
	open := time.Now().Before(c.openUntil)
	c.mu.Unlock()
	if open {
		return tool.Result{}, fmt.Errorf("%w: server %q failed %d times in a row", ErrCircuitOpen, c.cfg.Name, c.cfg.BreakAfter)
	}
	var args any
	if len(input) > 0 {
		args = input
	}
	rounds := 1
	if idempotent {
		rounds += c.cfg.MaxRetries
	}
	var err error
	for range rounds {
		var sess *sdk.ClientSession
		var res *sdk.CallToolResult
		if sess, err = c.connected(ctx); err == nil {
			if res, err = sess.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args}); err == nil {
				c.healthy()
				return result(res)
			}
			if rejected := (*jsonrpc.Error)(nil); errors.As(err, &rejected) {
				c.healthy()
				return tool.Result{}, err
			}
			c.drop(sess)
		}
		if ctx.Err() != nil {
			break
		}
	}
	c.failed()
	return tool.Result{}, fmt.Errorf("mcp server %q: %w", c.cfg.Name, err)
}

func result(res *sdk.CallToolResult) (tool.Result, error) {
	var text []string
	for _, content := range res.Content {
		if t, ok := content.(*sdk.TextContent); ok {
			text = append(text, t.Text)
		} else {
			text = append(text, "[non-text content omitted]")
		}
	}
	joined := strings.Join(text, "\n")
	if res.IsError {
		return tool.Result{}, errors.New(joined)
	}
	if res.StructuredContent != nil {
		out, err := json.Marshal(res.StructuredContent)
		return tool.Result{Output: out}, err
	}
	out, err := json.Marshal(map[string]string{"content": joined})
	return tool.Result{Output: out}, err
}

func (c *Client) connected(ctx context.Context) (*sdk.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	t, err := c.transport()
	if err != nil {
		return nil, err
	}
	sess, err := c.client.Connect(ctx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	c.session = sess
	return sess, nil
}

func (c *Client) drop(sess *sdk.ClientSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == sess {
		c.session = nil
		_ = sess.Close()
	}
}

func (c *Client) healthy() {
	c.mu.Lock()
	c.failures = 0
	c.mu.Unlock()
}

// failed counts a call that found no working connection. Until a call
// succeeds again, every further failure reopens the circuit.
func (c *Client) failed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures >= c.cfg.BreakAfter {
		c.openUntil = time.Now().Add(c.cfg.BreakFor)
	}
}

// Close ends the session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil
	return err
}
