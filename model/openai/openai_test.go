package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nvcnvn/agent-loop/model"
)

// Recorded from GLM 5.3 with thinking disabled: the content keeps a stray "</think>".
const glmPlain = `{"id":"a","choices":[{"index":0,"message":{"content":"</think>pong","role":"assistant","reasoning_content":null},"finish_reason":"stop"}],"model":"zai-org/GLM-5.3","object":"chat.completion","usage":{"prompt_tokens":18,"completion_tokens":3,"total_tokens":21}}`

// Recorded from GLM 5.3 with thinking on.
const glmToolCall = `{"id":"b","choices":[{"index":0,"message":{"content":null,"tool_calls":[{"id":"call_8f","type":"function","function":{"name":"math_add","arguments":"{\"a\": 17, \"b\": 25}"}}],"role":"assistant","reasoning_content":"The user wants me to add."},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":175,"completion_tokens":37,"total_tokens":212,"prompt_tokens_details":{"cached_tokens":100}}}`

func TestDecodeStripsInlineThinking(t *testing.T) {
	resp, err := decodeResponse([]byte(glmPlain))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "pong" || resp.Message.Thinking != "" {
		t.Fatalf("content=%q thinking=%q", resp.Message.Content, resp.Message.Thinking)
	}
	if resp.Usage.PromptTokens != 18 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 21 || resp.Usage.ModelCalls != 1 {
		t.Fatalf("usage=%+v", resp.Usage)
	}

	inline, _ := decodeResponse([]byte(`{"choices":[{"message":{"content":"<think>hmm</think>\n\nanswer"}}]}`))
	if inline.Message.Thinking != "hmm" || inline.Message.Content != "answer" {
		t.Fatalf("inline=%+v", inline.Message)
	}

	const quoted = "Close it with </think> after the reasoning."
	literal, _ := decodeResponse([]byte(`{"choices":[{"message":{"content":"` + quoted + `"}}]}`))
	if literal.Message.Content != quoted || literal.Message.Thinking != "" {
		t.Fatalf("literal=%+v", literal.Message)
	}
}

func TestDecodeToolCallReasoningAndCachedTokens(t *testing.T) {
	resp, err := decodeResponse([]byte(glmToolCall))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_8f" || resp.ToolCalls[0].Name != "math_add" || string(resp.ToolCalls[0].Input) != `{"a": 17, "b": 25}` {
		t.Fatalf("calls=%+v", resp.ToolCalls)
	}
	if resp.Message.Thinking != "The user wants me to add." || resp.Message.Content != "" {
		t.Fatalf("message=%+v", resp.Message)
	}
	if resp.Usage.CachedTokens != 100 {
		t.Fatalf("usage=%+v", resp.Usage)
	}
}

func TestSchemaAnswerFiledAsReasoningBecomesContent(t *testing.T) {
	const raw = `{"choices":[{"message":{"content":null,"reasoning_content":"{ \"qty\": 3 }"}}]}`
	for _, c := range []struct {
		schema  string
		content string
	}{{`{"type":"object"}`, `{ "qty": 3 }`}, {"", ""}} {
		resp, _ := decodeResponse([]byte(raw))
		answerInReasoning(&resp, model.ChatRequest{ResponseSchema: json.RawMessage(c.schema)})
		if resp.Message.Content != c.content {
			t.Fatalf("schema=%q content=%q", c.schema, resp.Message.Content)
		}
	}
}

func TestMalformedArgumentsBecomeAJSONString(t *testing.T) {
	resp, err := decodeResponse([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{\"a\":"}},{"function":{"name":"y","arguments":""}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.ToolCalls[0].Input); got != `"{\"a\":"` {
		t.Fatalf("malformed input=%s", got)
	}
	if resp.ToolCalls[0].ID != "call_0" || string(resp.ToolCalls[1].Input) != `{}` {
		t.Fatalf("calls=%+v", resp.ToolCalls)
	}
}

func TestEncodeRequest(t *testing.T) {
	p := New("http://x/v1", "")
	p.Extra = map[string]any{"thinking": map[string]any{"type": "disabled"}}
	buf, err := p.encodeRequest(model.ChatRequest{
		Model: "glm",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "look", Images: []model.Image{{MimeType: "image/png", Data: []byte{1, 2}}}},
			{Role: model.RoleAssistant, Thinking: "secret", ToolCalls: []model.ToolCall{{ID: "c1", Name: "math.add", Input: json.RawMessage(`{"a":1}`)}}},
			{Role: model.RoleTool, Name: "math.add", ToolCallID: "c1", Content: `{"sum":1}`},
		},
		Tools:          []model.ToolSpec{{Name: "math.add", Description: "sum", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolCallMode:   "auto",
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
		Parameters:     map[string]any{"max_tokens": 5, "num_ctx": 8192},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"max_tokens":5,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AQI="}}]},` +
		`{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"math_add","arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","content":"{\"sum\":1}","tool_call_id":"c1"}],` +
		`"model":"glm",` +
		`"response_format":{"json_schema":{"name":"output","schema":{"type":"object"}},"type":"json_schema"},` +
		`"thinking":{"type":"disabled"},"tool_choice":"auto",` +
		`"tools":[{"function":{"description":"sum","name":"math_add","parameters":{"type":"object"}},"type":"function"}]}`
	if string(buf) != want {
		t.Fatalf("got  %s\nwant %s", buf, want)
	}
}

func TestChatSendsKeyPricesAndReportsStatus(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, glmToolCall)
	}))
	defer srv.Close()
	p := New(srv.URL+"/v1/", "k")
	p.Price = Price{Input: 1e6, CachedInput: 1e5, Output: 2e6}

	resp, err := p.Chat(context.Background(), model.ChatRequest{Model: "glm", Tools: []model.ToolSpec{{Name: "math.add"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ToolCalls[0].Name != "math.add" || resp.Message.ToolCalls[0].Name != "math.add" {
		t.Fatalf("tool name not restored: %+v", resp.ToolCalls)
	}
	if want := int64(75 + 10 + 74); resp.Usage.CostUnits != want {
		t.Fatalf("cost=%d want %d", resp.Usage.CostUnits, want)
	}

	status = http.StatusTooManyRequests
	_, err = p.Chat(context.Background(), model.ChatRequest{Model: "glm"})
	var se *model.StatusError
	if !errors.As(err, &se) || se.Code != 429 || se.RetryAfter.Seconds() != 7 || !model.Transient(err) {
		t.Fatalf("err=%v", err)
	}
}

func FuzzDecodeResponse(f *testing.F) {
	f.Add([]byte(glmPlain))
	f.Add([]byte(glmToolCall))
	f.Add([]byte(`{"choices":[]}`))
	f.Add([]byte(`{"choices":[{"message":{"content":"</think></think><think>"}}]}`))
	f.Add([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"arguments":"\u0000"}}]}}]}`))
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
	})
}
