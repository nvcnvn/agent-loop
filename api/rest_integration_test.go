//go:build rest
// +build rest

// REST integration test against a running agent-loop HTTP server.
//
// Run locally with Docker Compose:
//
//	docker compose -f docker-compose.test.yml up --build --abort-on-container-exit --exit-code-from rest-test rest-test
package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
)

func TestRESTIntegrationRunAndSSE(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("AGENT_LOOP_REST_URL"), "/")
	if baseURL == "" {
		t.Skip("AGENT_LOOP_REST_URL not set")
	}
	timeout := restTimeout(t)
	client := &http.Client{Timeout: timeout}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	waitForHealth(ctx, t, client, baseURL)

	agents := restJSON[struct {
		Agents []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"agents"`
	}](ctx, t, client, http.MethodGet, baseURL+"/v1/debug/agents", nil)
	if len(agents.Agents) == 0 || agents.Agents[0].Name == "" || agents.Agents[0].Model == "" {
		t.Fatalf("debug agents missing setup: %+v", agents.Agents)
	}

	tools := restJSON[struct {
		Tools []struct {
			Name        string `json:"name"`
			Concurrency string `json:"concurrency"`
		} `json:"tools"`
	}](ctx, t, client, http.MethodGet, baseURL+"/v1/debug/tools", nil)
	if len(tools.Tools) == 0 {
		t.Fatal("debug tools returned no tools")
	}

	session := restJSON[struct {
		SessionID core.SessionID `json:"session_id"`
	}](ctx, t, client, http.MethodPost, baseURL+"/v1/sessions", map[string]any{"title": "rest integration"})
	if session.SessionID == "" {
		t.Fatal("empty session_id")
	}

	message := restJSON[struct {
		Event core.Event `json:"event"`
	}](ctx, t, client, http.MethodPost, baseURL+"/v1/sessions/"+string(session.SessionID)+"/messages", map[string]any{
		"role":    "human",
		"content": "Run a REST integration check and reply briefly.",
	})
	if message.Event.ID == "" {
		t.Fatal("message event missing id")
	}

	run := restJSON[struct {
		RunID core.RunID `json:"run_id"`
	}](ctx, t, client, http.MethodPost, baseURL+"/v1/runs", map[string]any{
		"session_id": session.SessionID,
		"workflow": map[string]any{
			"name": "rest-integration",
			"stages": []map[string]any{{
				"name": "reply",
				"kind": "sequential",
				"agents": []map[string]any{{
					"name":   agents.Agents[0].Name,
					"prompt": "Reply with one concise sentence confirming the REST integration path works.",
				}},
			}},
		},
		"limits": map[string]any{
			"max_model_calls": 8,
			"max_tool_calls":  8,
			"max_concurrent":  2,
			"max_runtime":     int64(15 * time.Minute),
		},
	})
	if run.RunID == "" {
		t.Fatal("empty run_id")
	}

	status := readRunSSE(ctx, t, client, baseURL, run.RunID)
	if status != string(core.RunSucceeded) {
		t.Fatalf("terminal status=%s, want %s", status, core.RunSucceeded)
	}

	events := restJSON[struct {
		Events []core.Event `json:"events"`
	}](ctx, t, client, http.MethodGet, baseURL+"/v1/runs/"+string(run.RunID)+"/events", nil)
	if len(events.Events) == 0 {
		t.Fatal("no run events returned")
	}
}

func restTimeout(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("AGENT_LOOP_REST_TIMEOUT")
	if raw == "" {
		return 5 * time.Minute
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		t.Fatalf("invalid AGENT_LOOP_REST_TIMEOUT=%q", raw)
	}
	return time.Duration(seconds) * time.Second
}

func waitForHealth(ctx context.Context, t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("server health timeout: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func restJSON[T any](ctx context.Context, t *testing.T, client *http.Client, method, url string, body any) T {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("%s %s status=%d body=%s", method, url, resp.StatusCode, raw)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s decode: %v body=%s", method, url, err, raw)
	}
	return out
}

func readRunSSE(ctx context.Context, t *testing.T, client *http.Client, baseURL string, runID core.RunID) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/runs/%s/stream", baseURL, runID), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE status=%d body=%s", resp.StatusCode, raw)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var eventName string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if eventName != core.EvRunStateChanged {
			continue
		}
		var envelope struct {
			Payload struct {
				Status string `json:"status"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); err != nil {
			t.Fatalf("SSE decode: %v", err)
		}
		if envelope.Payload.Status == string(core.RunSucceeded) || envelope.Payload.Status == string(core.RunFailed) || envelope.Payload.Status == string(core.RunCanceled) || envelope.Payload.Status == string(core.RunTimedOut) {
			return envelope.Payload.Status
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.Fatalf("SSE read: %v", err)
	}
	t.Fatalf("SSE ended without terminal state")
	return ""
}
