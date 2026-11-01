package scenario_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/model/openai"
	"github.com/nvcnvn/agent-loop/runtime"
)

// Acceptance: §Observability and Operations #7 – a run survives the network to its model dropping a
// connection before replying and cutting a reply off midway.
func TestModelNetworkLossIsRetried(t *testing.T) {
	const reply = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"all good\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rc := http.NewResponseController(w)
		switch requests.Add(1) {
		case 1:
			if conn, _, err := rc.Hijack(); err == nil {
				_ = conn.Close()
			}
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(reply)))
			_, _ = io.WriteString(w, reply[:20])
			_ = rc.Flush()
			if conn, _, err := rc.Hijack(); err == nil {
				_ = conn.Close()
			}
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, reply)
		}
	}))
	defer srv.Close()
	prov := &model.Retry{Provider: openai.New(srv.URL+"/v1/", "k"), Attempts: 4, Base: time.Millisecond, Max: 5 * time.Millisecond}

	res, events := run(t, []runtime.Agent{{Name: "ag", Model: "m", Provider: prov}},
		runtime.Stage{Name: "s", Agents: []runtime.AgentStep{{Name: "ag", Prompt: "status?"}}})
	if res.Outputs["s"] != "all good" || requests.Load() != 3 {
		t.Fatalf("outputs=%v after %d requests", res.Outputs, requests.Load())
	}
	completed := 0
	for _, ev := range events {
		if ev.Type == core.EvModelCompleted {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("%d model calls recorded; want the one that succeeded", completed)
	}
}
