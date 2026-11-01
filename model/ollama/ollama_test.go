package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nvcnvn/agent-loop/model"
)

const toolCallResponse = `{"model":"gemma4","done":true,"prompt_eval_count":30,"eval_count":12,
"message":{"role":"assistant","content":"","tool_calls":[
 {"function":{"name":"math.add","arguments":{"a":19,"b":23}}},
 {"function":{"name":"echo"}}]}}`

func TestEncodeRequest(t *testing.T) {
	buf, err := encodeRequest(model.ChatRequest{
		Model: "gemma4",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "add"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call_0", Name: "math.add", Input: json.RawMessage(`{"a":1}`)}}},
			{Role: model.RoleTool, Name: "math.add", ToolCallID: "call_0", Content: `{"sum":1}`},
		},
		Tools:          []model.ToolSpec{{Name: "math.add", Description: "sum", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Parameters:     map[string]any{"temperature": 0, "max_tokens": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gemma4","messages":[` +
		`{"role":"user","content":"add"},` +
		`{"role":"assistant","content":"","tool_calls":[{"function":{"name":"math.add","arguments":{"a":1}}}]},` +
		`{"role":"tool","content":"{\"sum\":1}","tool_name":"math.add","tool_call_id":"call_0"}],` +
		`"stream":false,` +
		`"tools":[{"function":{"description":"sum","name":"math.add","parameters":{"type":"object"}},"type":"function"}],` +
		`"options":{"num_predict":5,"temperature":0},"format":{"type":"object"}}`
	if string(buf) != want {
		t.Fatalf("got  %s\nwant %s", buf, want)
	}
}

func TestDecodeToolCalls(t *testing.T) {
	resp, err := decodeResponse([]byte(toolCallResponse))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls=%d", len(resp.ToolCalls))
	}
	first, second := resp.ToolCalls[0], resp.ToolCalls[1]
	if first.ID != "call_0" || first.Name != "math.add" || string(first.Input) != `{"a":19,"b":23}` {
		t.Fatalf("first=%+v input=%s", first, first.Input)
	}
	if second.ID != "call_1" || string(second.Input) != `{}` {
		t.Fatalf("second=%+v input=%s", second, second.Input)
	}
	if u := resp.Usage; u.PromptTokens != 30 || u.CompletionTokens != 12 || u.TotalTokens != 42 || u.ModelCalls != 1 {
		t.Fatalf("usage=%+v", u)
	}
}

func TestChatOverHTTP(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if strings.Contains(gotBody, "fail") {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(toolCallResponse))
	}))
	defer srv.Close()
	p := New(srv.URL, srv.Client())

	resp, err := p.Chat(context.Background(), model.ChatRequest{Model: "gemma4", Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/chat" || !strings.Contains(gotBody, `"stream":false`) {
		t.Fatalf("path=%s body=%s", gotPath, gotBody)
	}
	if len(resp.ToolCalls) != 2 || string(resp.Raw) != toolCallResponse {
		t.Fatalf("resp=%+v", resp)
	}

	_, err = p.Chat(context.Background(), model.ChatRequest{Model: "fail"})
	if err == nil || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("err=%v", err)
	}
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add([]byte(toolCallResponse))
	f.Add([]byte(`{"message":{"role":"assistant","content":"hi"}}`))
	f.Add([]byte(`{"message":{"tool_calls":[{"function":{"name":"x","arguments":null}}]}}`))
	f.Add([]byte(`{"message":{"tool_calls":[{"function":{"arguments":"not-an-object"}}]}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		resp, err := decodeResponse(raw)
		if err != nil {
			return
		}
		for _, tc := range resp.ToolCalls {
			if !json.Valid(tc.Input) {
				t.Fatalf("invalid tool input %q", tc.Input)
			}
		}
		if _, err := encodeRequest(model.ChatRequest{Messages: []model.Message{resp.Message}}); err != nil {
			t.Fatalf("decoded message does not re-encode: %v", err)
		}
	})
}

func TestEncodeImages(t *testing.T) {
	buf, err := encodeRequest(model.ChatRequest{Model: "m", Messages: []model.Message{
		{Role: model.RoleUser, Content: "what is this?", Images: []model.Image{{MimeType: "image/png", Data: []byte("png!")}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), `"images":["cG5nIQ=="]`) {
		t.Fatalf("images not sent as base64: %s", buf)
	}
}

func TestCapabilitiesFromShow(t *testing.T) {
	shows := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			http.NotFound(w, r)
			return
		}
		shows++
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		caps := `["completion"]`
		if req.Model == "vl" {
			caps = `["completion","vision","tools","thinking"]`
		}
		_, _ = io.WriteString(w, `{"capabilities":`+caps+`}`)
	}))
	defer srv.Close()
	p := New(srv.URL, srv.Client())
	for range 2 {
		c, err := p.Capabilities(context.Background(), "vl")
		if err != nil || !c.Vision || !c.ToolCalling {
			t.Fatalf("vl caps=%+v err=%v", c, err)
		}
	}
	if c, err := p.Capabilities(context.Background(), "text"); err != nil || c.Vision || c.ToolCalling {
		t.Fatalf("text caps=%+v err=%v", c, err)
	}
	if shows != 2 {
		t.Fatalf("asked the server %d times, want once per model", shows)
	}
}

func TestThinkingRoundTrips(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"message":{"role":"assistant","content":"2","thinking":"one plus one"}}`))
	if err != nil || resp.Message.Thinking != "one plus one" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	buf, err := encodeRequest(model.ChatRequest{Model: "m", Messages: []model.Message{resp.Message}})
	if err != nil || !strings.Contains(string(buf), `"thinking":"one plus one"`) {
		t.Fatalf("thinking not sent back: %s err=%v", buf, err)
	}
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("not streaming: %s", body)
		}
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","thinking":"hm"},"done":false}
{"message":{"role":"assistant","content":"4"},"done":false}
{"message":{"role":"assistant","content":"2","tool_calls":[{"function":{"name":"echo","arguments":{"x":1}}}]},"done":false}
{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":7,"eval_count":3}
`)
	}))
	defer srv.Close()
	var deltas []model.Delta
	resp, err := New(srv.URL, nil).ChatStream(context.Background(), model.ChatRequest{Model: "m"}, func(d model.Delta) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "42" || resp.Message.Thinking != "hm" || len(resp.ToolCalls) != 1 || resp.Usage.TotalTokens != 10 {
		t.Fatalf("resp=%+v", resp)
	}
	if len(deltas) != 3 || deltas[0].Thinking != "hm" || deltas[1].Content != "4" {
		t.Fatalf("deltas=%+v", deltas)
	}
}

func FuzzServerReplies(f *testing.F) {
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	p := New(srv.URL, srv.Client())
	f.Add(`{"capabilities":["completion","tools","vision"]}`)
	f.Add(`{"embeddings":[[0.1,0.2],[0.3]]}`)
	f.Add(`{"embeddings":null}`)
	f.Add(`{"message":{"content":"a"}}` + "\n" + `{"message":{"tool_calls":[{"function":{"name":"x","arguments":"[]"}}]},"done":true}`)
	f.Add(`{"message":{"content":"a"}}{`)
	var n atomic.Int64
	f.Fuzz(func(t *testing.T, raw string) {
		body.Store(raw)
		ctx := context.Background()
		var show struct{ Capabilities []string }
		if caps, err := p.Capabilities(ctx, fmt.Sprint("m", n.Add(1))); err == nil && json.Unmarshal([]byte(raw), &show) == nil && caps.Vision != slices.Contains(show.Capabilities, "vision") {
			t.Fatalf("vision=%v from %q", caps.Vision, raw)
		}
		if vecs, err := p.Embed(ctx, "m", []string{"a", "b"}); err == nil && len(vecs) != 2 {
			t.Fatalf("%d vectors for 2 texts", len(vecs))
		}
		var deltas strings.Builder
		resp, err := p.ChatStream(ctx, model.ChatRequest{Model: "m"}, func(d model.Delta) { deltas.WriteString(d.Content) })
		if err != nil {
			return
		}
		if deltas.String() != resp.Message.Content {
			t.Fatalf("deltas %q do not add up to %q", deltas.String(), resp.Message.Content)
		}
		for _, tc := range resp.ToolCalls {
			if !json.Valid(tc.Input) {
				t.Fatalf("invalid tool input %q", tc.Input)
			}
		}
	})
}
