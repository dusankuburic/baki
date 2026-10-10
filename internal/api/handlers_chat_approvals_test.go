package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// seedFixableFlow stores a flow whose one block trips unhandled-error, which has a non-behaviour-changing
// auto-fix (wrap-error-handler): the one shape apply_fix accepts through chat.
func seedFixableFlow(t *testing.T, rt *Router, id, ownerID string) {
	t.Helper()
	content := `{"id":"` + id + `","name":"t","subflows":[` +
		`{"id":"sf-main","name":"Main","blocks":[` +
		`{"id":"b1","name":"Call API","type":"ACTION","rawType":"HTTPClient.InvokeUrl","subflowId":"sf-main","lineNumber":3,` +
		`"properties":{"Url":"https://x"}}` +
		`]}]}`
	doc := &storageif.FlowDocument{ID: id, Name: "t", OwnerID: ownerID, Content: json.RawMessage(content)}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

// resumeBody is the /api/chat/resume answer: the journal carries the approval events a client replays.
type resumeBody struct {
	Done   bool   `json:"done"`
	Error  string `json:"error"`
	Events []struct {
		Type string                 `json:"type"`
		Data map[string]interface{} `json:"data"`
	} `json:"events"`
}

// pollResume re-reads the stream until ok accepts the body, or fails after 5 s.
func pollResume(t *testing.T, rt *Router, bearer, streamID string, ok func(resumeBody) bool) resumeBody {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/resume", bearer, map[string]string{"id": streamID})
		checkStatus(t, rr, http.StatusOK)
		var body resumeBody
		decodeJSON(t, rr, &body)
		if ok(body) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream never reached the expected state; last body %s", rr.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (b resumeBody) decisionStatuses() []string {
	var out []string
	for _, ev := range b.Events {
		if ev.Type == "fix_decision" {
			st, _ := ev.Data["status"].(string)
			out = append(out, st)
		}
	}
	return out
}

// TestApprovals_RoundTripThroughRouter drives an approval end to end through the router: a chat+write API
// token starts a stream whose model calls apply_fix, the proposal reaches the journal with the generic envelope
// (tool, previewKind, requiredScope, items[]), a chat-only token of the same user is refused at the route, another
// user is refused as not owning the stream, and the owner's decision on POST /api/chat/fix-decision resolves the
// card and the fix is applied.
func TestApprovals_RoundTripThroughRouter(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Done.", TokensIn: 8, TokensOut: 3},
	}}
	ai.RegisterProvider("approval-router", func(string) ai.Provider { return p })

	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-owner", "owner@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "u-other", "other@acme.io", auth.RoleMember)
	seedFixableFlow(t, rt, "f-approval", "u-owner")
	ownerSession := jwtBearer(t, rt, "u-owner", "owner@acme.io")
	chatWrite := mintPAT(t, rt, ownerSession, []string{auth.ScopeChat, auth.ScopeWrite})
	chatOnly := mintPAT(t, rt, ownerSession, []string{auth.ScopeChat})
	other := jwtBearer(t, rt, "u-other", "other@acme.io")

	const streamID = "77777777-7777-4777-8777-777777777777"
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/stream", chatWrite, map[string]any{
		"flowId": "f-approval", "provider": "approval-router", "userMessage": "fix it", "useTools": true,
		"clientStreamId": streamID,
	})
	checkStatus(t, rr, http.StatusOK)

	body := pollResume(t, rt, chatWrite, streamID, func(b resumeBody) bool {
		for _, ev := range b.Events {
			if ev.Type == "fix_proposal" {
				return true
			}
		}
		return b.Done || b.Error != ""
	})
	var proposal map[string]interface{}
	for _, ev := range body.Events {
		if ev.Type == "fix_proposal" {
			proposal = ev.Data
		}
	}
	if proposal == nil {
		t.Fatalf("no proposal: done=%v error=%q events=%+v", body.Done, body.Error, body.Events)
	}
	proposalID, _ := proposal["proposalId"].(string)
	if proposalID == "" {
		t.Fatalf("proposal without an id: %v", proposal)
	}
	// The generic envelope the ApprovalCard renders by, next to the flat fields old clients read.
	if proposal["tool"] != "apply_fix" || proposal["previewKind"] != ai.PreviewKindFixPatch || proposal["requiredScope"] != auth.ScopeWrite {
		t.Errorf("proposal envelope = %v", proposal)
	}
	items, _ := proposal["items"].([]interface{})
	if len(items) != 1 || proposal["ruleId"] != "unhandled-error" {
		t.Errorf("proposal items = %v (ruleId %v), want one item and the flat fields", proposal["items"], proposal["ruleId"])
	}

	decision := map[string]any{"streamId": streamID, "proposalId": proposalID, "approved": true}
	var refused struct {
		Code string `json:"code"`
	}
	// A chat-only token of the same user: the route needs write on top of chat (the authz matrix shows the
	// same 403 for an unknown stream, which is the ownership check below; the code tells them apart).
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/fix-decision", chatOnly, decision)
	checkStatus(t, rr, http.StatusForbidden)
	decodeJSON(t, rr, &refused)
	if refused.Code == codeStreamNotFound {
		t.Errorf("a chat-only token was refused as not owning the stream; the scope gate must answer first")
	}
	// Another user: not the stream's owner, and not told whether it exists.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/fix-decision", other, decision)
	checkStatus(t, rr, http.StatusForbidden)
	decodeJSON(t, rr, &refused)
	if refused.Code != codeStreamNotFound {
		t.Errorf("another user's decision answered code %q, want %s", refused.Code, codeStreamNotFound)
	}
	// The card is still pending: the owner's decision wins.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/fix-decision", chatWrite, decision)
	checkStatus(t, rr, http.StatusOK)

	body = pollResume(t, rt, chatWrite, streamID, func(b resumeBody) bool { return b.Done || b.Error != "" })
	if body.Error != "" {
		t.Fatalf("stream failed: %s", body.Error)
	}
	statuses := body.decisionStatuses()
	if len(statuses) != 2 || statuses[0] != "applying" || statuses[1] != "applied" {
		t.Errorf("decision statuses = %v, want [applying applied]", statuses)
	}
	// Decided: a second decision finds nothing pending.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/fix-decision", chatWrite, decision)
	checkStatus(t, rr, http.StatusNotFound)
}
