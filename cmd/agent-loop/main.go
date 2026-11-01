// Command agent-loop runs the agent-loop runtime over HTTP. By default it uses
// the in-memory store and mock model provider; environment variables switch to
// PostgreSQL, Ollama, or an OpenAI-compatible endpoint (LLM_BASE_URL, LLM_MODEL).
//
// Usage:
//
//	go run ./cmd/agent-loop -debug -trust-identity-headers
//
// Then:
//
//	curl -X POST http://localhost:8080/v1/sessions -d '{"title":"demo"}'
//	curl -X POST http://localhost:8080/v1/runs \
//	  -H 'Content-Type: application/json' \
//	  -d '{"session_id":"<id>","workflow":{"name":"demo","stages":[{"name":"reply","kind":"sequential","agents":[{"name":"echo","prompt":"Say hi"}]}]}}'
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/ollama"
	"github.com/nvcnvn/agent-loop/model/openai"
	"github.com/nvcnvn/agent-loop/observe"
	"github.com/nvcnvn/agent-loop/policy"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	pgstore "github.com/nvcnvn/agent-loop/store/pg"
	"github.com/nvcnvn/agent-loop/tool"
	"github.com/nvcnvn/agent-loop/tool/mcp"
	"github.com/nvcnvn/agent-loop/tool/memory"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	debug := flag.Bool("debug", false, "serve the debug console and /v1/debug/* (exposes agent instructions)")
	trustHeaders := flag.Bool("trust-identity-headers", false, "take the tenant and principal from X-Tenant-ID/X-Principal-ID without authentication (local development only)")
	configPath := flag.String("config", os.Getenv("AGENT_LOOP_CONFIG"), "JSON file of agents, workflow templates, compaction, tool policy, limits, worker pool and MCP servers, with ${VAR} read from the environment (see README); default $AGENT_LOOP_CONFIG; without one the server has one echo agent")
	drain := flag.Duration("shutdown-timeout", 30*time.Second, "on SIGINT/SIGTERM, how long to let requests and runs finish before checkpointing runs")
	flag.Parse()
	redactor, err := observe.NewRedactor(observe.DefaultRedactPatterns...)
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"LLM_API_KEY", "AGENT_LOOP_PG_DSN"} {
		redactor.Add(os.Getenv(name))
	}
	slog.SetDefault(slog.New(redactor.Handler(slog.NewJSONHandler(os.Stdout, nil))))
	auth := authenticator(*trustHeaders, redactor)
	stopTracing := setupTracing()

	persistence := openStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		fatal("register tools", err)
	}

	memories := registerMemory(reg, persistence)
	prov, modelName := openProvider()
	cfg := fileConfig{Config: runtime.Config{Agents: []runtime.Agent{{
		Name:         "echo",
		Role:         "demo",
		Instructions: "Local debug agent for REST, SSE, persistence, and model-provider smoke tests.",
		Tools:        []string{"echo", "uppercase", "math.add"},
	}}}}
	if *configPath != "" {
		cfg = loadConfig(*configPath)
	}
	for _, p := range cfg.Redact {
		if err := redactor.AddPattern(p); err != nil {
			fatal("config redact", err)
		}
	}
	for _, s := range cfg.MCPServers {
		for k, v := range s.Headers {
			if credentialHeader(k) {
				redactor.Add(v)
			}
		}
	}
	mcpClients := connectMCP(cfg.MCPServers, reg)
	for i := range cfg.Agents {
		cfg.Agents[i].Provider = prov
		if cfg.Agents[i].Model == "" {
			cfg.Agents[i].Model = modelName
		}
	}
	orch := runtime.NewOrchestrator(persistence, reg, cfg.Agents)
	orch.Provider, orch.Model = prov, modelName
	cfg.Apply(orch)
	orch.Redactor, orch.Secrets = redactor, toolSecrets()
	if n, err := orch.Recover(context.Background()); err != nil {
		slog.Error("recover runs", "resumed", n, "err", err)
	} else if n > 0 {
		slog.Info("resumed unfinished runs", "count", n)
	}

	purging, stopPurging := context.WithCancel(context.Background())
	defer stopPurging()
	if orch.Retention != (runtime.Retention{}) {
		go orch.KeepPurging(purging, time.Hour)
	}

	srv := api.NewServer(orch, persistence)
	srv.Debug = *debug
	srv.Auth = auth
	if memories != nil {
		srv.Memory = memories
	}
	maps.Copy(srv.Visibility, cfg.Visibility)
	hs := srv.HTTPServer(*addr)
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.ListenAndServe() }()
	slog.Info("listening", "addr", *addr, "debug", *debug)
	select {
	case err := <-serveErr:
		fatal("serve", err)
	case <-signals.Done():
	}

	slog.Info("shutting down", "timeout", *drain)
	ctx, cancel := context.WithTimeout(context.Background(), *drain)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		slog.Error("http shutdown", "err", err)
	}
	if err := orch.Shutdown(ctx); err != nil {
		slog.Warn("runs still executing were checkpointed for the next start", "err", err)
	}
	for _, c := range mcpClients {
		_ = c.Close()
	}
	stopTracing(ctx)
}

// fileConfig is the -config file: runtime agents and workflows, and the MCP
// servers whose tools are registered next to the builtin ones.
type fileConfig struct {
	runtime.Config
	MCPServers []mcp.Server `json:"mcp_servers,omitempty"`
	// Visibility replaces, for each role it lists, the event visibilities that role reads.
	Visibility policy.Visibility `json:"visibility,omitempty"`
	// Redact lists more Go regular expressions whose matches logs and traces hide.
	Redact []string `json:"redact,omitempty"`
}

// credentialHeader reports whether an HTTP header's value is a credential to redact.
func credentialHeader(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "auth") || strings.Contains(name, "key") || strings.Contains(name, "token") ||
		strings.Contains(name, "secret") || strings.Contains(name, "cookie")
}

// toolSecrets reads tools' credentials from the files of AGENT_LOOP_SECRETS_DIR
// when it is set, and otherwise from AGENT_LOOP_SECRET_<NAME> variables.
func toolSecrets() tool.Secrets {
	if dir := os.Getenv("AGENT_LOOP_SECRETS_DIR"); dir != "" {
		return tool.DirSecrets{Dir: dir}
	}
	return tool.EnvSecrets{Prefix: "AGENT_LOOP_SECRET_"}
}

func loadConfig(path string) fileConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		fatal("read config", err)
	}
	if data, err = runtime.ExpandEnv(data); err != nil {
		fatal("config "+path, err)
	}
	var cfg fileConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		fatal("config "+path, err)
	}
	if err := cfg.Validate(); err != nil {
		fatal("config "+path, err)
	}
	if err := cfg.Visibility.Validate(); err != nil {
		fatal("config "+path, err)
	}
	slog.Info("loaded config", "path", path, "agents", len(cfg.Agents), "workflows", len(cfg.Workflows), "mcp_servers", len(cfg.MCPServers))
	return cfg
}

func connectMCP(servers []mcp.Server, reg *tool.Registry) []*mcp.Client {
	var clients []*mcp.Client
	seen := map[string]bool{}
	for _, s := range servers {
		if seen[s.Name] {
			fatal("mcp", fmt.Errorf("mcp server name %q is used twice", s.Name))
		}
		seen[s.Name] = true
		c, err := mcp.New(s)
		if err != nil {
			fatal("mcp", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		names, err := c.Register(ctx, reg)
		cancel()
		if err != nil {
			fatal("mcp server "+s.Name, err)
		}
		slog.Info("registered MCP tools", "server", s.Name, "tools", names)
		clients = append(clients, c)
	}
	return clients
}

// setupTracing exports spans over OTLP/HTTP when an OTLP endpoint is set in
// the standard OTEL_EXPORTER_OTLP_* environment variables.
func setupTracing() func(context.Context) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) {}
	}
	exporter, err := otlptracehttp.New(context.Background())
	if err != nil {
		fatal("otlp exporter", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	otel.SetTracerProvider(tp)
	slog.Info("exporting traces over OTLP")
	return func(ctx context.Context) {
		if err := tp.Shutdown(ctx); err != nil {
			slog.Error("flush traces", "err", err)
		}
	}
}

func authenticator(trustHeaders bool, redactor *observe.Redactor) api.Authenticator {
	if spec := os.Getenv("AGENT_LOOP_API_KEYS"); spec != "" {
		keys, err := api.ParseAPIKeys(spec)
		if err != nil {
			fatal("AGENT_LOOP_API_KEYS", err)
		}
		for key := range keys {
			redactor.Add(key)
		}
		slog.Info("authenticating API keys", "keys", len(keys))
		return api.APIKeys(keys)
	}
	if !trustHeaders {
		fatal("authentication", errors.New("set AGENT_LOOP_API_KEYS or pass -trust-identity-headers"))
	}
	slog.Warn("trusting identity headers without authentication")
	return api.HeaderIdentity
}

func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

func openStore() store.Store {
	dsn := os.Getenv("AGENT_LOOP_PG_DSN")
	if dsn == "" {
		slog.Info("using in-memory store")
		return store.NewMemStore()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	persistence, err := pgstore.New(ctx, dsn)
	if err != nil {
		fatal("postgres connect", err)
	}
	if err := persistence.Migrate(ctx); err != nil {
		fatal("postgres migrate", err)
	}
	slog.Info("using PostgreSQL store")
	return persistence
}

// registerMemory adds the memory tools when an embedding model is configured:
// LLM_EMBED_MODEL on the OpenAI-compatible endpoint, or OLLAMA_EMBED_MODEL on
// Ollama. MEMORY_WRITE_SCOPE (session, project, workflow or tenant) is where agents save.
func registerMemory(reg *tool.Registry, s store.Store) *memory.Tools {
	t := memory.Tools{Store: s, WriteScope: store.MemoryScope(os.Getenv("MEMORY_WRITE_SCOPE"))}
	switch {
	case os.Getenv("LLM_EMBED_MODEL") != "":
		t.Model, t.Embedder = os.Getenv("LLM_EMBED_MODEL"), model.WithRetry(openai.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY")))
	case os.Getenv("OLLAMA_EMBED_MODEL") != "":
		t.Model, t.Embedder = os.Getenv("OLLAMA_EMBED_MODEL"), model.WithRetry(ollama.New(cmp.Or(os.Getenv("OLLAMA_BASE_URL"), os.Getenv("OLLAMA_URL")), nil))
	default:
		return nil
	}
	switch t.WriteScope {
	case "", store.MemorySession, store.MemoryProject, store.MemoryWorkflow, store.MemoryTenant:
	default:
		fatal("MEMORY_WRITE_SCOPE", fmt.Errorf("%q is not session, project, workflow or tenant", t.WriteScope))
	}
	if err := t.Register(reg); err != nil {
		fatal("register memory tools", err)
	}
	slog.Info("memory tools enabled", "embed_model", t.Model, "write_scope", cmp.Or(t.WriteScope, store.MemorySession))
	return &t
}

func openProvider() (model.Provider, string) {
	if name := os.Getenv("LLM_MODEL"); name != "" && os.Getenv("LLM_BASE_URL") != "" {
		p := openai.New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"))
		if raw := os.Getenv("LLM_PRICE"); raw != "" {
			if _, err := fmt.Sscanf(raw, "%g,%g,%g", &p.Price.Input, &p.Price.CachedInput, &p.Price.Output); err != nil {
				fatal("invalid LLM_PRICE, want input,cached_input,output per million tokens", err)
			}
		}
		slog.Info("using OpenAI-compatible endpoint", "model", name, "base_url", p.BaseURL)
		return model.WithRetry(p), name
	}
	modelName := os.Getenv("OLLAMA_MODEL")
	if modelName == "" {
		return model.NewMockProvider(model.MockResponse{Content: "Hello from agent-loop!"}), "mock"
	}
	baseURL := os.Getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		baseURL = os.Getenv("OLLAMA_URL")
	}
	timeout := 60 * time.Second
	if raw := os.Getenv("OLLAMA_TIMEOUT_SECONDS"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			fatal("invalid OLLAMA_TIMEOUT_SECONDS", fmt.Errorf("%q", raw))
		}
		timeout = time.Duration(seconds) * time.Second
	}
	slog.Info("using Ollama", "model", modelName, "timeout", timeout)
	return model.WithRetry(ollama.New(baseURL, &http.Client{Timeout: timeout})), modelName
}
