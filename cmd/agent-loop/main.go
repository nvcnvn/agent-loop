// Command agent-loop runs the agent-loop runtime over HTTP. By default it uses
// the in-memory store and mock model provider; environment variables can switch
// local testing to PostgreSQL and Ollama.
//
// Usage:
//
//	go run ./cmd/agent-loop -debug
//
// Then:
//
//	curl -X POST http://localhost:8080/v1/sessions -d '{"title":"demo"}'
//	curl -X POST http://localhost:8080/v1/runs \
//	  -H 'Content-Type: application/json' \
//	  -d '{"session_id":"<id>","workflow":{"name":"demo","stages":[{"name":"reply","kind":"sequential","agents":[{"name":"echo","prompt":"Say hi"}]}]}}'
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/ollama"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	pgstore "github.com/nvcnvn/agent-loop/store/pg"
	"github.com/nvcnvn/agent-loop/tool"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	debug := flag.Bool("debug", false, "serve the debug console and /v1/debug/* (exposes agent instructions)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	persistence := openStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		fatal("register tools", err)
	}

	prov, modelName := openProvider()
	agent := runtime.Agent{
		Name:         "echo",
		Role:         "demo",
		Instructions: "Local debug agent for REST, SSE, persistence, and model-provider smoke tests.",
		Model:        modelName,
		Provider:     prov,
		Tools:        []string{"echo", "uppercase", "math.add"},
	}
	orch := runtime.NewOrchestrator(persistence, reg, []runtime.Agent{agent})
	if n, err := orch.Recover(context.Background()); err != nil {
		slog.Error("recover runs", "resumed", n, "err", err)
	} else if n > 0 {
		slog.Info("resumed unfinished runs", "count", n)
	}

	srv := api.NewServer(orch, persistence)
	srv.Debug = *debug
	slog.Info("listening", "addr", *addr, "debug", *debug)
	if err := srv.HTTPServer(*addr).ListenAndServe(); err != nil {
		fatal("serve", err)
	}
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

func openProvider() (model.Provider, string) {
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
	return ollama.New(baseURL, &http.Client{Timeout: timeout}), modelName
}
