//go:build ollama
// +build ollama

// Integration test against a local Ollama instance.
//
// Run with:
//
//	OLLAMA_MODEL=gemma4:latest go test -tags=ollama ./model/...
//
// The build tag keeps this test out of the default `go test ./...` run so
// CI does not need an Ollama server.
package model_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/model"
)

func TestOllamaChat(t *testing.T) {
	mdl := os.Getenv("OLLAMA_MODEL")
	if mdl == "" {
		t.Skip("OLLAMA_MODEL not set")
	}
	prov := model.NewOllamaProvider(os.Getenv("OLLAMA_URL"), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := prov.Chat(ctx, model.ChatRequest{
		Model: mdl,
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "Reply with the single word: OK"},
		},
	})
	if err != nil {
		t.Fatalf("ollama chat: %v", err)
	}
	if resp.Message.Content == "" {
		t.Fatal("empty response")
	}
	t.Logf("ollama said: %q (tokens=%d)", resp.Message.Content, resp.Usage.TotalTokens)
}
