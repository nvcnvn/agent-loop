// Command agent-loop runs the agent-loop runtime over HTTP. By default it uses
// the in-memory store and mock model provider; environment variables can switch
// local testing to PostgreSQL and Ollama.
//
// Usage:
//
//	go run ./cmd/agent-loop
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
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/nvcnvn/agent-loop/api"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	pgstore "github.com/nvcnvn/agent-loop/store/pg"
	"github.com/nvcnvn/agent-loop/tool"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	persistence := openStore()
	reg := tool.NewRegistry()
	if err := tool.RegisterBuiltins(reg); err != nil {
		log.Fatal(err)
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

	srv := api.NewServer(orch, persistence)
	log.Printf("agent-loop listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func openStore() store.Store {
	dsn := os.Getenv("AGENT_LOOP_PG_DSN")
	if dsn == "" {
		log.Print("using in-memory store")
		return store.NewMemStore()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	persistence, err := pgstore.New(ctx, dsn)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	if err := persistence.EnsureSchema(ctx); err != nil {
		log.Fatalf("postgres schema: %v", err)
	}
	log.Print("using PostgreSQL store")
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
			log.Fatalf("invalid OLLAMA_TIMEOUT_SECONDS=%q", raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}
	log.Printf("using Ollama model %s with timeout %s", modelName, timeout)
	return model.NewOllamaProvider(baseURL, &http.Client{Timeout: timeout}), modelName
}
