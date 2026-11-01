package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/runtime"
	"github.com/nvcnvn/agent-loop/store"
	"github.com/nvcnvn/agent-loop/tool"
)

func everyRouteServer() *Server {
	s := store.NewMemStore()
	srv := NewServer(runtime.NewOrchestrator(s, tool.NewRegistry(), nil), s)
	srv.Debug = true
	srv.Auth = APIKeys(nil)
	return srv
}

func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Fatalf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	var documented []string
	for path, item := range doc.Paths {
		for method, raw := range item {
			if method == "parameters" {
				continue
			}
			pattern := strings.ToUpper(method) + " " + path
			documented = append(documented, pattern)
			var op struct {
				Stability string             `json:"x-stability"`
				Security  *[]json.RawMessage `json:"security"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s: %v", pattern, err)
			}
			if op.Stability != "stable" && op.Stability != "internal" {
				t.Errorf("%s: x-stability = %q, want stable or internal", pattern, op.Stability)
			}
			if public := op.Security != nil && len(*op.Security) == 0; public != publicRoutes[pattern] {
				t.Errorf("%s: documented without identity = %v, served without one = %v", pattern, public, publicRoutes[pattern])
			}
		}
	}
	var registered []string
	for pattern := range everyRouteServer().routes() {
		registered = append(registered, pattern)
	}
	slices.Sort(documented)
	slices.Sort(registered)
	if !slices.Equal(documented, registered) {
		t.Fatalf("documented operations differ from registered routes\ndocumented: %q\nregistered: %q", documented, registered)
	}
}

func TestOpenAPIIsServedWithoutIdentity(t *testing.T) {
	rec := httptest.NewRecorder()
	everyRouteServer().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/openapi.json", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" || !bytes.Equal(body, openAPI) {
		t.Fatalf("status %d, content type %q, body is the document: %v", rec.Code, rec.Header().Get("Content-Type"), bytes.Equal(body, openAPI))
	}
}
