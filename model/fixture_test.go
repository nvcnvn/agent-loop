package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nvcnvn/agent-loop/model"
)

func chat(content string) model.ChatRequest {
	return model.ChatRequest{
		Model:    "m",
		Messages: []model.Message{{Role: model.RoleUser, Content: content}},
		Tools:    []model.ToolSpec{{Name: "echo", InputSchema: json.RawMessage(`{ "type": "object" }`)}},
	}
}

func TestRecordedFixtureReplaysByRequestIgnoringIDsAndModel(t *testing.T) {
	ctx := context.Background()
	rec := model.NewRecorder(model.NewMockProvider(
		model.MockResponse{Content: "a1"},
		model.MockResponse{Content: "b"},
		model.MockResponse{Content: "a2"},
	))
	for _, c := range []string{"a", "b", "a"} {
		req := chat(c)
		req.RunID, req.StepID, req.IdempotencyKey = "run-1", "step-1", "k"
		if _, err := rec.Chat(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "fx.json")
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}

	rep, err := model.LoadReplayer(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{{"b", "b"}, {"a", "a1"}, {"a", "a2"}} {
		req := chat(tc.in)
		req.Model, req.RunID, req.StepID = "other-model", "run-2", "step-2"
		resp, err := rep.Chat(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if resp.Message.Content != tc.want {
			t.Fatalf("%s: got %q want %q", tc.in, resp.Message.Content, tc.want)
		}
	}
	if n := rep.Remaining(); n != 0 {
		t.Fatalf("remaining=%d", n)
	}
	if _, err := rep.Chat(ctx, chat("a")); !errors.Is(err, model.ErrNoFixture) {
		t.Fatalf("exhausted: err=%v want ErrNoFixture", err)
	}
	if _, err := rep.Chat(ctx, chat("changed prompt")); !errors.Is(err, model.ErrNoFixture) {
		t.Fatalf("prompt drift: err=%v want ErrNoFixture", err)
	}
}

func TestRecorderSkipsFailedCalls(t *testing.T) {
	boom := errors.New("boom")
	rec := model.NewRecorder(model.NewMockProvider(model.MockResponse{Err: boom}))
	if _, err := rec.Chat(context.Background(), chat("a")); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	path := filepath.Join(t.TempDir(), "fx.json")
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}
	rep, err := model.LoadReplayer(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := rep.Remaining(); n != 0 {
		t.Fatalf("remaining=%d", n)
	}
}
