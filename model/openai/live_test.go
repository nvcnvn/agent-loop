package openai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/model"
)

// liveProvider returns the .env endpoint, skipping unless paid tests are allowed.
func liveProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	if os.Getenv("ALLOW_PAID_LLM") != "1" || os.Getenv("LLM_BASE_URL") == "" {
		t.Skip("paid: set ALLOW_PAID_LLM=1 and LLM_* (make test-paid)")
	}
	return New(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY")), os.Getenv("LLM_MODEL")
}

func TestLiveChatAndStream(t *testing.T) {
	p, name := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := model.ChatRequest{Model: name, Messages: []model.Message{{Role: model.RoleUser, Content: "Count from 1 to 5, separated by spaces, nothing else."}}}

	resp, err := p.Chat(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Message.Content, "1 2 3 4 5") || resp.Usage.PromptTokens == 0 {
		t.Fatalf("chat: %+v", resp)
	}

	var deltas []string
	resp, err = p.ChatStream(ctx, req, func(d model.Delta) {
		if d.Content != "" {
			deltas = append(deltas, d.Content)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(strings.Join(deltas, "")) != resp.Message.Content || !strings.Contains(resp.Message.Content, "1 2 3 4 5") {
		t.Fatalf("stream: content=%q deltas=%q", resp.Message.Content, deltas)
	}
	if resp.Usage.PromptTokens == 0 {
		t.Fatalf("stream reported no usage: %+v", resp.Usage)
	}
	t.Logf("%d content deltas, usage %+v", len(deltas), resp.Usage)
}
