package scenario_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/tool"
	"github.com/nvcnvn/agent-loop/tool/mcp"
)

// The support-desk world: fixed data, so tool outputs and therefore the
// recorded requests are the same on every run.

type order struct {
	ID       string  `json:"order_id"`
	Item     string  `json:"item"`
	Status   string  `json:"status"`
	Total    float64 `json:"total"`
	DaysLate int     `json:"days_late,omitempty"`
}

var (
	customers = map[string]map[string]string{
		"ada@example.com": {"customer_id": "C-17", "name": "Ada Lovelace"},
		"bob@example.com": {"customer_id": "C-42", "name": "Bob Stone"},
	}
	orders = map[string][]order{
		"C-17": {
			{ID: "O-301", Item: "keyboard", Status: "delivered", Total: 89},
			{ID: "O-302", Item: "monitor", Status: "late", Total: 240, DaysLate: 9},
		},
		"C-42": {{ID: "O-410", Item: "desk lamp", Status: "delivered", Total: 35}},
	}
	stock  = map[string]int{"SKU-7731": 42, "SKU-1200": 7}
	prices = map[string]float64{"SKU-7731": 15, "SKU-1200": 40}
	kb     = []struct{ Title, Text string }{
		{"Shipping times", "Standard shipping takes 5 business days. Express shipping takes 2 business days and costs $15."},
		{"Returns and refunds", "Items can be returned within 30 days of delivery for a full refund. Late orders can be refunded at once."},
		{"Warranty", "Electronics carry a 1-year limited warranty."},
	}
	skuPattern = regexp.MustCompile(`^SKU-\d+$`)
)

func schema(properties string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":` + string(req) + `}`)
}

// jsonTool registers a tool whose input decodes into In.
func jsonTool[In any](t *testing.T, reg *tool.Registry, def core.ToolDefinition, fn func(In) (any, error)) {
	t.Helper()
	exec := tool.ExecutorFunc(func(_ context.Context, call tool.Call) (tool.Result, error) {
		var in In
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tool.Result{}, fmt.Errorf("invalid input: %w", err)
		}
		out, err := fn(in)
		if err != nil {
			return tool.Result{}, err
		}
		raw, err := json.Marshal(out)
		return tool.Result{Output: raw}, err
	})
	if def.Concurrency == "" {
		def.Concurrency = core.ConcurrencyParallelRead
		def.Idempotency = core.IdemIdempotent
	}
	if err := reg.Register(def, exec); err != nil {
		t.Fatal(err)
	}
}

// supportTools registers the CRM, order, inventory, catalog, knowledge base
// and style-check tools.
func supportTools(t *testing.T, reg *tool.Registry) {
	jsonTool(t, reg, core.ToolDefinition{
		Name: "crm.find_customer", Description: "Find a customer by email address.",
		InputSchema: schema(`"email":{"type":"string"}`, "email"),
	}, func(in struct{ Email string }) (any, error) {
		c, ok := customers[strings.ToLower(strings.TrimSpace(in.Email))]
		if !ok {
			return nil, fmt.Errorf("no customer with email %q", in.Email)
		}
		return c, nil
	})
	jsonTool(t, reg, core.ToolDefinition{
		Name: "orders.list", Description: "List a customer's orders by customer ID.",
		InputSchema: schema(`"customer_id":{"type":"string"}`, "customer_id"),
	}, func(in struct {
		CustomerID string `json:"customer_id"`
	}) (any, error) {
		list, ok := orders[in.CustomerID]
		if !ok {
			return nil, fmt.Errorf("no customer with ID %q", in.CustomerID)
		}
		return map[string]any{"orders": list}, nil
	})
	refunded := map[string]bool{}
	jsonTool(t, reg, core.ToolDefinition{
		Name: "orders.refund", Description: "Refund an order in full. Needs a human's approval.",
		InputSchema:      schema(`"order_id":{"type":"string"},"reason":{"type":"string"}`, "order_id", "reason"),
		SideEffect:       core.SideWriteExternal,
		Idempotency:      core.IdemNonIdempotent,
		Concurrency:      core.ConcurrencySerial,
		RequiresApproval: true,
	}, func(in struct {
		OrderID string `json:"order_id"`
	}) (any, error) {
		for _, list := range orders {
			for _, o := range list {
				if o.ID == in.OrderID {
					if refunded[o.ID] {
						return nil, fmt.Errorf("order %s was already refunded (R-9001)", o.ID)
					}
					refunded[o.ID] = true
					return map[string]any{"refund_id": "R-9001", "order_id": o.ID, "amount": o.Total}, nil
				}
			}
		}
		return nil, fmt.Errorf("no order with ID %q", in.OrderID)
	})
	jsonTool(t, reg, core.ToolDefinition{
		Name: "inventory.stock", Description: "Units in stock for a SKU.",
		InputSchema: schema(`"sku":{"type":"string"}`, "sku"),
	}, func(in struct{ SKU string }) (any, error) {
		if !skuPattern.MatchString(in.SKU) {
			return nil, fmt.Errorf("unknown sku %q: SKUs look like SKU-7731", in.SKU)
		}
		n, ok := stock[in.SKU]
		if !ok {
			return nil, fmt.Errorf("unknown sku %q", in.SKU)
		}
		return map[string]any{"sku": in.SKU, "in_stock": n}, nil
	})
	jsonTool(t, reg, core.ToolDefinition{
		Name: "catalog.price", Description: "Unit price in dollars of a SKU.",
		InputSchema: schema(`"sku":{"type":"string"}`, "sku"),
	}, func(in struct{ SKU string }) (any, error) {
		p, ok := prices[in.SKU]
		if !ok {
			return nil, fmt.Errorf("unknown sku %q", in.SKU)
		}
		return map[string]any{"sku": in.SKU, "price": p}, nil
	})
	jsonTool(t, reg, core.ToolDefinition{
		Name: "kb.search", Description: "Search the help-center knowledge base. Returns the best matching article.",
		InputSchema: schema(`"query":{"type":"string"}`, "query"),
	}, func(in struct{ Query string }) (any, error) {
		best, score := -1, 0
		for i, a := range kb {
			text := strings.ToLower(a.Title + " " + a.Text)
			n := 0
			for _, w := range strings.Fields(strings.ToLower(in.Query)) {
				if len(w) > 2 && strings.Contains(text, strings.Trim(w, "?.,!")) {
					n++
				}
			}
			if n > score {
				best, score = i, n
			}
		}
		if best < 0 {
			return map[string]any{"results": []any{}}, nil
		}
		return map[string]any{"title": kb[best].Title, "text": kb[best].Text}, nil
	})
	jsonTool(t, reg, core.ToolDefinition{
		Name: "style.check", Description: "Check a customer reply against the support style guide.",
		InputSchema: schema(`"text":{"type":"string"}`, "text"),
	}, func(in struct{ Text string }) (any, error) {
		problems := []string{}
		if !strings.Contains(in.Text, "T-1042") {
			problems = append(problems, "it must mention the ticket number T-1042")
		}
		lower := strings.ToLower(in.Text)
		if !strings.Contains(lower, "sorry") && !strings.Contains(lower, "apolog") {
			problems = append(problems, "it must apologise")
		}
		if n := len(strings.Fields(in.Text)); n > 80 {
			problems = append(problems, fmt.Sprintf("it has %d words; the limit is 80", n))
		}
		return map[string]any{"ok": len(problems) == 0, "problems": problems}, nil
	})
}

type noteQuery struct {
	Query string `json:"query" jsonschema:"words to look for"`
}

type note struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type noteResults struct {
	Notes []note `json:"notes"`
}

// notesServer registers, through the MCP adapter, the tools of a notes MCP
// server reached over streamable HTTP.
func notesServer(t *testing.T, reg *tool.Registry) {
	t.Helper()
	notes := []note{
		{Title: "Q3 offsite", Body: "The Q3 offsite is on 14 November in Lisbon. Flights are booked by the travel team."},
		{Title: "Holiday party", Body: "The holiday party is on 19 December at the office."},
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "notes", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "search", Description: "Search the team's notes.", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *sdk.CallToolRequest, in noteQuery) (*sdk.CallToolResult, noteResults, error) {
			var found []note
			for _, n := range notes {
				for _, w := range strings.Fields(strings.ToLower(in.Query)) {
					if len(w) > 2 && strings.Contains(strings.ToLower(n.Title+" "+n.Body), w) && !slices.Contains(found, n) {
						found = append(found, n)
					}
				}
			}
			return nil, noteResults{Notes: found}, nil
		})
	httpSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
	t.Cleanup(httpSrv.Close)
	c, err := mcp.New(mcp.Server{Name: "notes", URL: httpSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
}
