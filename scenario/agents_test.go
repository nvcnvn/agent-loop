package scenario_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/nvcnvn/agent-loop/core"
	"github.com/nvcnvn/agent-loop/model"
	"github.com/nvcnvn/agent-loop/runtime"
)

const supportAgent = "You are a customer support agent. Use the tools to look things up and act; never invent IDs. " +
	"Call one tool at a time and wait for its result. Tools that need a manager's approval ask for it themselves when you call them, " +
	"so never ask the customer for approval. If a tool call comes back not approved, tell the customer it could not be done."

func step(agent, prompt string) []runtime.AgentStep {
	return []runtime.AgentStep{{Name: agent, Prompt: prompt}}
}

const refundRequest = "Customer ada@example.com writes: \"My monitor order is very late. I want my money back.\" " +
	"Find the customer, list their orders, and refund the late one. " +
	"Finish with one sentence for the customer that includes the refund ID."

// A chain of tool calls whose IDs flow from one result into the next call,
// ending in a write tool that waits for a human's approval.
func TestSupportRefund(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t, runtime.Agent{
			Name: "support", Model: name, Provider: prov, MaxToolLoops: 8, Instructions: supportAgent,
			Tools: []string{"crm.find_customer", "orders.list", "orders.refund"},
		})
		supportTools(t, w.reg)
		w.decision = "approved"
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "resolve", Agents: step("support", refundRequest)},
		}}})
		calls := toolCalls(t, events)
		refunds := callsTo(calls, "orders.refund")
		if len(refunds) == 0 || !strings.Contains(refunds[0].Input, "O-302") || refunds[0].Error != "" {
			t.Fatalf("refunds=%+v", refunds)
		}
		for _, again := range refunds[1:] {
			if !strings.Contains(again.Error, "already refunded") {
				t.Fatalf("a second refund went through: %+v", refunds)
			}
		}
		if lookups := callsTo(calls, "orders.list"); len(lookups) == 0 || !strings.Contains(lookups[0].Input, "C-17") {
			t.Fatalf("orders.list calls=%+v", lookups)
		}
		if !strings.Contains(res.Outputs["resolve"], "R-9001") {
			t.Fatalf("answer=%q", res.Outputs["resolve"])
		}
	})
}

// A rejected approval goes back to the agent, which must not claim a refund.
func TestRefundRejected(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t, runtime.Agent{
			Name: "support", Model: name, Provider: prov, MaxToolLoops: 8,
			Instructions: supportAgent,
			Tools:        []string{"crm.find_customer", "orders.list", "orders.refund"},
		})
		supportTools(t, w.reg)
		w.decision = "rejected"
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "resolve", Agents: step("support", refundRequest)},
		}}})
		calls := toolCalls(t, events)
		refunds := callsTo(calls, "orders.refund")
		if len(refunds) == 0 || !strings.Contains(refunds[0].Error, "not approved (rejected)") {
			t.Fatalf("calls=%+v answer=%q", calls, res.Outputs["resolve"])
		}
		if strings.Contains(res.Outputs["resolve"], "R-9001") {
			t.Fatalf("the agent claimed a refund that was rejected: %q", res.Outputs["resolve"])
		}
	})
}

// A tool error goes back to the model, which fixes its input and calls again.
func TestToolErrorRecovery(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t, runtime.Agent{
			Name: "stock", Model: name, Provider: prov, MaxToolLoops: 6, Tools: []string{"inventory.stock"},
			Instructions: "Answer stock questions with the inventory.stock tool. If a tool returns an error, read it, fix the input and call the tool again.",
		})
		supportTools(t, w.reg)
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "answer", Agents: step("stock", "How many units of item 7731 do we have in stock? Pass the item number to the tool as given.")},
		}}})
		calls := callsTo(toolCalls(t, events), "inventory.stock")
		if len(calls) < 2 || calls[0].Error == "" || calls[len(calls)-1].Output != `{"in_stock":42,"sku":"SKU-7731"}` {
			t.Fatalf("inventory.stock calls=%+v", calls)
		}
		if !strings.Contains(res.Outputs["answer"], "42") {
			t.Fatalf("answer=%q", res.Outputs["answer"])
		}
	})
}

// A lead agent with no tools delegates to a child agent that has one.
func TestDelegatedResearch(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t,
			runtime.Agent{
				Name: "lead", Model: name, Provider: prov, Children: []string{"librarian"}, MaxToolLoops: 6,
				Instructions: "You answer customers. You cannot look anything up yourself: give the question to the librarian with the delegate tool, then answer the customer in one sentence.",
			},
			runtime.Agent{
				Name: "librarian", Role: "searches the help-center knowledge base", Model: name, Provider: prov,
				Tools: []string{"kb.search"}, MaxToolLoops: 4,
				Instructions: "Search the knowledge base with kb.search and reply with the fact that answers the question.",
			},
		)
		supportTools(t, w.reg)
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "answer", Agents: step("lead", "A customer asks: how many business days does express shipping take?")},
		}}})
		calls := toolCalls(t, events)
		if len(callsTo(calls, "delegate")) == 0 || len(callsTo(calls, "kb.search")) == 0 {
			t.Fatalf("calls=%+v", calls)
		}
		tasks, err := w.store.ListRunTasks(context.Background(), core.DefaultTenant, res.RunID)
		if err != nil {
			t.Fatal(err)
		}
		child := false
		for _, task := range tasks {
			child = child || (task.AgentName == "librarian" && task.ParentTaskID != "" && task.Depth == 1)
		}
		if !child || !strings.Contains(res.Outputs["answer"], "2") {
			t.Fatalf("child task found=%v answer=%q", child, res.Outputs["answer"])
		}
	})
}

// The reviewer checks the draft with a tool and hands it back with the
// problems. The check needs a ticket number only the feedback gives, so the
// first draft always goes back.
func TestDraftReviewHandBack(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t,
			runtime.Agent{Name: "writer", Model: name, Provider: prov, Instructions: "You write short, polite replies to customers. Reply with only the text of the reply."},
			runtime.Agent{
				Name: "reviewer", Model: name, Provider: prov, Tools: []string{"style.check"}, MaxToolLoops: 4,
				Instructions: "Call style.check once with the draft text exactly as given. If the result has ok true, reply with exactly APPROVED. " +
					"Otherwise reply with REJECTED: followed by the problems it lists.",
			},
		)
		supportTools(t, w.reg)
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "draft", Agents: step("writer", "Write a reply of at most three sentences to a customer whose monitor delivery is late.\n"+
				"Reviewer feedback on your previous draft, if any: {{feedback}}")},
			{Name: "review", HandBack: &runtime.HandBack{When: "(?i)rejected", To: "draft", MaxTimes: 3},
				Agents: step("reviewer", "Check this draft:\n\n{{stage.draft}}")},
		}}})
		if handBacks := countSystemEvents(t, events, "stage_handed_back"); handBacks == 0 {
			t.Fatal("the first draft was not handed back")
		}
		checks := callsTo(toolCalls(t, events), "style.check")
		if len(checks) < 2 || !strings.Contains(checks[len(checks)-1].Output, `"ok":true`) {
			t.Fatalf("style.check calls=%+v", checks)
		}
		if !strings.Contains(res.Outputs["draft"], "T-1042") {
			t.Fatalf("final draft=%q", res.Outputs["draft"])
		}
	})
}

// A structured-output stage extracts an order as JSON, and the next stage
// prices it with parallel tool calls.
func TestOrderExtractionThenQuote(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t,
			runtime.Agent{Name: "clerk", Model: name, Provider: prov, Instructions: "You turn emails into order data."},
			runtime.Agent{
				Name: "pricer", Model: name, Provider: prov, Tools: []string{"catalog.price"}, MaxToolLoops: 6,
				Instructions: "Look up the unit price of every SKU with catalog.price, then reply with only the order total in dollars.",
			},
		)
		supportTools(t, w.reg)
		orderSchema := json.RawMessage(`{"type":"object","properties":{` +
			`"customer_email":{"type":"string"},` +
			`"items":{"type":"array","items":{"type":"object","properties":{"sku":{"type":"string"},"quantity":{"type":"integer"}},"required":["sku","quantity"]}}},` +
			`"required":["customer_email","items"]}`)
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "extract", OutputSchema: orderSchema, Agents: step("clerk",
				"Extract the order in this email:\n\nFrom: bob@example.com\nHi! Please send me 2 x SKU-7731 and 1 x SKU-1200. Thanks, Bob")},
			{Name: "quote", Agents: step("pricer", "Order: {{stage.extract}}")},
		}}})
		var extracted struct {
			Email string `json:"customer_email"`
			Items []struct {
				SKU      string `json:"sku"`
				Quantity int    `json:"quantity"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(res.Outputs["extract"]), &extracted); err != nil {
			t.Fatalf("extract=%q: %v", res.Outputs["extract"], err)
		}
		got := map[string]int{}
		for _, it := range extracted.Items {
			got[it.SKU] += it.Quantity
		}
		if extracted.Email != "bob@example.com" || got["SKU-7731"] != 2 || got["SKU-1200"] != 1 {
			t.Fatalf("extracted=%+v", extracted)
		}
		if n := len(callsTo(toolCalls(t, events), "catalog.price")); n < 2 {
			t.Fatalf("catalog.price called %d times", n)
		}
		if !strings.Contains(res.Outputs["quote"], "70") {
			t.Fatalf("quote=%q", res.Outputs["quote"])
		}
	})
}

// A classifier routes email through a graph: spam stops the run, support
// mail fans out to two branches that a writer joins.
func TestTriageGraph(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t,
			runtime.Agent{Name: "classifier", Model: name, Provider: prov, Instructions: "You classify incoming email for a shop's support team."},
			runtime.Agent{Name: "accounts", Model: name, Provider: prov, MaxToolLoops: 6, Instructions: supportAgent,
				Tools: []string{"crm.find_customer", "orders.list"}},
			runtime.Agent{Name: "librarian", Model: name, Provider: prov, Tools: []string{"kb.search"},
				Instructions: "Search the knowledge base with kb.search and reply with the fact that answers the question."},
			runtime.Agent{Name: "writer", Model: name, Provider: prov, Instructions: "You write short, polite replies to customers."},
		)
		supportTools(t, w.reg)
		wf := runtime.Workflow{Name: "triage", Stages: []runtime.Stage{
			{Name: "triage", StopWhen: `"category"\s*:\s*"spam"`,
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"category":{"type":"string","enum":["spam","support"]}},"required":["category"]}`),
				Agents:       step("classifier", "Classify this email as spam or support:\n\n{{input.email}}")},
			{Name: "account", DependsOn: []string{"triage"}, Agents: step("accounts", "Find the customer {{input.from}}, list their orders, and reply with the IDs of the late ones.")},
			{Name: "policy", DependsOn: []string{"triage"}, Agents: step("librarian", "What is the policy that applies to this email?\n\n{{input.email}}")},
			{Name: "reply", DependsOn: []string{"account", "policy"}, Agents: step("writer",
				"Write a two-sentence reply to this email:\n{{input.email}}\n\nAccount findings: {{stage.account}}\nPolicy: {{stage.policy}}")},
		}}

		spam, _ := w.run(runtime.StartRunInput{Workflow: wf, Input: map[string]string{
			"from": "promo@lucky.example", "email": "CONGRATULATIONS!!! You won a free cruise. Click here to claim your prize now!"}})
		if _, ran := spam.Outputs["reply"]; ran || !strings.Contains(spam.Outputs["triage"], "spam") {
			t.Fatalf("spam run outputs=%v", spam.Outputs)
		}

		support, events := w.run(runtime.StartRunInput{Workflow: wf, Input: map[string]string{
			"from": "ada@example.com", "email": "Hello, my monitor order is late. Can I get a refund? - Ada"}})
		if !strings.Contains(support.Outputs["triage"], "support") || !strings.Contains(support.Outputs["account"], "O-302") || support.Outputs["reply"] == "" {
			t.Fatalf("support run outputs=%v", support.Outputs)
		}
		if len(callsTo(toolCalls(t, events), "kb.search")) == 0 {
			t.Fatal("the policy branch did not search the knowledge base")
		}
	})
}

// Later runs in a session see its earlier human messages and answers.
func TestSessionMemory(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t, runtime.Agent{Name: "support", Model: name, Provider: prov, Instructions: "You are a customer support agent. Be brief."})
		turn := runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "reply", Agents: step("support", "Reply to the customer's last message.")},
		}}}
		w.say("Hi, I'm Ada. My order number is O-302 and it hasn't arrived.")
		w.run(turn)
		w.say("Sorry, what was my order number again? Reply with just the number.")
		res, _ := w.run(turn)
		if !strings.Contains(res.Outputs["reply"], "O-302") && !strings.Contains(res.Outputs["reply"], "302") {
			t.Fatalf("reply=%q", res.Outputs["reply"])
		}
	})
}

// An MCP server's tool, reached over streamable HTTP, answers the question.
func TestMCPNotes(t *testing.T) {
	forEachModel(t, false, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t, runtime.Agent{
			Name: "assistant", Model: name, Provider: prov, Tools: []string{"notes.search"}, MaxToolLoops: 4,
			Instructions: "You are a team assistant. Look facts up in the team's notes with notes.search before answering.",
		})
		notesServer(t, w.reg)
		res, events := w.run(runtime.StartRunInput{Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "answer", Agents: step("assistant", "When and where is the Q3 offsite? Answer in one sentence.")},
		}}})
		searches := callsTo(toolCalls(t, events), "notes.search")
		if len(searches) == 0 || !strings.Contains(searches[len(searches)-1].Output, "Lisbon") {
			t.Fatalf("notes.search calls=%+v", searches)
		}
		if answer := res.Outputs["answer"]; !strings.Contains(answer, "14") || !strings.Contains(answer, "Lisbon") {
			t.Fatalf("answer=%q", answer)
		}
	})
}

// A vision stage turns the image into JSON that a tool-using stage adds up.
func TestImageToJSONThenTool(t *testing.T) {
	forEachModel(t, true, func(t *testing.T, prov model.Provider, name string) {
		w := newWorld(t,
			runtime.Agent{Name: "looker", Model: name, Provider: prov, Instructions: "You describe images precisely."},
			runtime.Agent{Name: "adder", Model: name, Provider: prov, Tools: []string{"math.add"},
				Instructions: "Always use the math.add tool for arithmetic. After the tool result, answer with only the number."},
		)
		res, events := w.run(runtime.StartRunInput{Images: shapesImage(t), Workflow: runtime.Workflow{Stages: []runtime.Stage{
			{Name: "see", OutputSchema: json.RawMessage(`{"type":"object","properties":{"circles":{"type":"integer"},"squares":{"type":"integer"}},"required":["circles","squares"]}`),
				Agents: []runtime.AgentStep{{Name: "looker", Prompt: "Count the circles and the squares in this image.", Images: []string{"shapes"}}}},
			{Name: "total", Agents: step("adder", "Shape counts: {{stage.see}}. Add the two counts.")},
		}}})
		var counts struct{ Circles, Squares int }
		if err := json.Unmarshal([]byte(res.Outputs["see"]), &counts); err != nil || counts.Circles != 2 || counts.Squares != 3 {
			t.Fatalf("see=%q err=%v", res.Outputs["see"], err)
		}
		if adds := callsTo(toolCalls(t, events), "math.add"); len(adds) == 0 || adds[len(adds)-1].Output != `{"sum":5}` {
			t.Fatalf("math.add calls=%+v", adds)
		}
		if !regexp.MustCompile(`\b5\b`).MatchString(res.Outputs["total"]) {
			t.Fatalf("total=%q", res.Outputs["total"])
		}
	})
}

func countSystemEvents(t *testing.T, events []core.Event, name string) int {
	t.Helper()
	n := 0
	for _, ev := range events {
		var p struct {
			Event string `json:"event"`
		}
		if ev.Type == core.EvSystem && json.Unmarshal(ev.Payload, &p) == nil && p.Event == name {
			n++
		}
	}
	return n
}
