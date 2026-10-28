package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/drain"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// seedThreeFixableFlow stores a flow with three blocks that each trip unhandled-error (auto-fix
// wrap-error-handler), for the batch cases. They carry the name the parser gives the action, so the three are
// identical siblings told apart only by their place, before and after each fix re-parses the flow.
func seedThreeFixableFlow(t *testing.T, rt *Router, id, ownerID string) {
	t.Helper()
	block := func(bid, url string, line int) string {
		return fmt.Sprintf(`{"id":%q,"name":"Invoke Url","type":"ACTION","rawType":"HTTPClient.InvokeUrl","subflowId":"sf-main","lineNumber":%d,"properties":{"Url":%q}}`, bid, line, url)
	}
	content := `{"id":"` + id + `","name":"t","subflows":[{"id":"sf-main","name":"Main","blocks":[` +
		block("b1", "https://a", 3) + `,` + block("b2", "https://b", 5) + `,` + block("b3", "https://c", 7) + `]}]}`
	doc := &storageif.FlowDocument{ID: id, Name: "t", OwnerID: ownerID, Content: json.RawMessage(content)}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

// approvalCase is one stream of the router table: its own provider, flow and stream id.
type approvalCase struct {
	rt       *Router
	bearer   string
	streamID string
	flowID   string
	p        *testutil.FakeProvider
}

func startApprovalCase(t *testing.T, provider, flowID, streamID string, call ai.ToolCall, seed func(*testing.T, *Router, string, string), prep func(*Router)) approvalCase {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{call}, TokensIn: 10, TokensOut: 5},
		{Text: "Final answer.", TokensIn: 8, TokensOut: 3},
	}}
	ai.RegisterProvider(provider, func(string) ai.Provider { return p })
	rt := newJWTTestRouter(t)
	if prep != nil {
		prep(rt)
	}
	seedUserWithRole(t, rt, "u-owner", "owner@acme.io", auth.RoleMember)
	seed(t, rt, flowID, "u-owner")
	session := jwtBearer(t, rt, "u-owner", "owner@acme.io")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/stream", session, map[string]any{
		"flowId": flowID, "provider": provider, "userMessage": "fix it", "useTools": true, "clientStreamId": streamID,
	})
	checkStatus(t, rr, http.StatusOK)
	return approvalCase{rt: rt, bearer: session, streamID: streamID, flowID: flowID, p: p}
}

// proposal waits for the card and returns its event data.
func (c approvalCase) proposal(t *testing.T) map[string]interface{} {
	t.Helper()
	body := pollResume(t, c.rt, c.bearer, c.streamID, func(b resumeBody) bool {
		return b.event("fix_proposal") != nil || b.Done || b.Error != ""
	})
	prop := body.event("fix_proposal")
	if prop == nil {
		t.Fatalf("no proposal: done=%v error=%q", body.Done, body.Error)
	}
	return prop
}

func (c approvalCase) decide(t *testing.T, body map[string]any) int {
	t.Helper()
	body["streamId"] = c.streamID
	return doRequestWithAuth(t, c.rt, http.MethodPost, "/api/chat/fix-decision", c.bearer, body).Code
}

func (c approvalCase) finished(t *testing.T) resumeBody {
	t.Helper()
	body := pollResume(t, c.rt, c.bearer, c.streamID, func(b resumeBody) bool { return b.Done || b.Error != "" })
	if body.Error != "" {
		t.Fatalf("stream failed: %s", body.Error)
	}
	return body
}

func (c approvalCase) content(t *testing.T) string {
	t.Helper()
	doc, err := c.rt.security.Backend.LoadFlow(context.Background(), c.flowID)
	if err != nil {
		t.Fatalf("load flow: %v", err)
	}
	return string(doc.Content)
}

// storedSource is the PAD source of a stored flow's content.
func storedSource(t *testing.T, content string) string {
	t.Helper()
	var doc struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("stored content: %v", err)
	}
	return doc.Source
}

func (b resumeBody) event(typ string) map[string]interface{} {
	for _, ev := range b.Events {
		if ev.Type == typ {
			return ev.Data
		}
	}
	return nil
}

// lastDecision is the card's terminal fix_decision.
func (b resumeBody) lastDecision() map[string]interface{} {
	var last map[string]interface{}
	for _, ev := range b.Events {
		if ev.Type == "fix_decision" {
			last = ev.Data
		}
	}
	return last
}

// TestApprovals_BatchOptOutThroughRouter: an apply_fixes card approved with excludedItemIndices [1] applies the
// first and third fix and reports the second as skipped, item by item and in order, and the model's next
// request reads the per-item result. Renaming the field on either side fails here.
func TestApprovals_BatchOptOutThroughRouter(t *testing.T) {
	call := ai.ToolCall{ID: "t1", Name: "apply_fixes", Input: []byte(`{"targets":[` +
		`{"blockId":"b1","ruleId":"unhandled-error"},{"blockId":"b2","ruleId":"unhandled-error"},{"blockId":"b3","ruleId":"unhandled-error"}]}`)}
	c := startApprovalCase(t, "approval-router-batch", "f-batch", "77777777-7777-4777-8777-777777777771", call, seedThreeFixableFlow, nil)
	prop := c.proposal(t)
	items, _ := prop["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("proposal items = %v, want 3", prop["items"])
	}
	ids := make([]string, 3)
	for i, it := range items {
		ids[i], _ = it.(map[string]interface{})["id"].(string)
	}
	before := c.content(t)
	decision := map[string]any{"proposalId": prop["proposalId"], "approved": true, "excludedItemIndices": []int{1}}
	if code := c.decide(t, decision); code != http.StatusOK {
		t.Fatalf("decision answered %d", code)
	}
	body := c.finished(t)
	dec := body.lastDecision()
	got, _ := dec["items"].([]interface{})
	want := []struct{ id, status string }{{ids[0], "applied"}, {ids[1], "skipped"}, {ids[2], "applied"}}
	if len(got) != len(want) {
		t.Fatalf("decision items = %v, want %v", dec["items"], want)
	}
	for i, w := range want {
		m, _ := got[i].(map[string]interface{})
		if m["id"] != w.id || m["status"] != w.status {
			t.Errorf("decision item %d = %v, want id %s status %s", i, m, w.id, w.status)
		}
	}
	after := c.content(t)
	if after == before {
		t.Error("the flow content did not change after an approved batch")
	}
	// The deselected middle block is the one left outside an error handler.
	if src := storedSource(t, after); !strings.Contains(src, "END\n        HTTPClient.InvokeUrl Url: https://b\n        ON BLOCK ERROR") {
		t.Errorf("the deselected block was fixed, or a selected one was not:\n%s", src)
	}
	reqs := c.p.Requests()
	next, _ := json.Marshal(reqs[len(reqs)-1])
	if !strings.Contains(string(next), "Batch result") || !strings.Contains(string(next), "skipped") {
		t.Errorf("the model's next request does not carry the per-item result: %s", next)
	}
	if code := c.decide(t, decision); code != http.StatusNotFound {
		t.Errorf("a second decision answered %d, want 404", code)
	}
}

// TestApprovals_DeclineThroughRouter: a declined card resolves declined, nothing changes and the model answers.
func TestApprovals_DeclineThroughRouter(t *testing.T) {
	call := ai.ToolCall{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}
	c := startApprovalCase(t, "approval-router-decline", "f-decline", "77777777-7777-4777-8777-777777777772", call, seedFixableFlow, nil)
	prop := c.proposal(t)
	before := c.content(t)
	decision := map[string]any{"proposalId": prop["proposalId"], "approved": false}
	if code := c.decide(t, decision); code != http.StatusOK {
		t.Fatalf("decision answered %d", code)
	}
	body := c.finished(t)
	if st := body.lastDecision()["status"]; st != "declined" {
		t.Errorf("decision status = %v, want declined", st)
	}
	if !body.Done {
		t.Error("the stream did not finish with an answer after the decline")
	}
	if c.content(t) != before {
		t.Error("a declined fix changed the flow")
	}
	if code := c.decide(t, decision); code != http.StatusNotFound {
		t.Errorf("a second decision answered %d, want 404", code)
	}
}

// TestApprovals_TimeoutThroughRouter: a card nobody decides resolves timeout once the (shortened) wait ends and
// the flow is unchanged; a late decision finds nothing pending.
func TestApprovals_TimeoutThroughRouter(t *testing.T) {
	call := ai.ToolCall{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}
	shorten := func(rt *Router) {
		moduleOf[*ChatHandler](t, rt).chatSvc.SetDecisionWaitForTesting(100 * time.Millisecond)
	}
	c := startApprovalCase(t, "approval-router-timeout", "f-timeout", "77777777-7777-4777-8777-777777777773", call, seedFixableFlow, shorten)
	prop := c.proposal(t)
	before := c.content(t)
	body := c.finished(t)
	if st := body.lastDecision()["status"]; st != "timeout" {
		t.Errorf("decision status = %v, want timeout", st)
	}
	if c.content(t) != before {
		t.Error("a timed-out card changed the flow")
	}
	if code := c.decide(t, map[string]any{"proposalId": prop["proposalId"], "approved": true}); code != http.StatusNotFound {
		t.Errorf("a decision after the timeout answered %d, want 404", code)
	}
}

// TestApprovals_DrainingRefusesAnApproveThroughRouter (R-approvals-c17): once the server drains, an approve is
// answered 503 SERVER_RESTARTING with Retry-After and applies nothing; a decline is still accepted.
func TestApprovals_DrainingRefusesAnApproveThroughRouter(t *testing.T) {
	call := ai.ToolCall{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}
	st := drain.New()
	c := startApprovalCase(t, "approval-router-drain", "f-drain-decide", "77777777-7777-4777-8777-777777777774", call, seedFixableFlow,
		func(rt *Router) { moduleOf[*ChatHandler](t, rt).chatSvc.SetDrainingForTesting(st) })
	prop := c.proposal(t)
	before := c.content(t)
	st.Begin()

	rr := doRequestWithAuth(t, c.rt, http.MethodPost, "/api/chat/fix-decision", c.bearer,
		map[string]any{"streamId": c.streamID, "proposalId": prop["proposalId"], "approved": true})
	checkServerRestarting(t, rr)
	if c.content(t) != before {
		t.Error("an approve refused while draining changed the flow")
	}
	if code := c.decide(t, map[string]any{"proposalId": prop["proposalId"], "approved": false}); code != http.StatusOK {
		t.Fatalf("a decline while draining answered %d, want 200", code)
	}
	if st := c.finished(t).lastDecision()["status"]; st != "declined" {
		t.Errorf("decision status = %v, want declined", st)
	}
}
