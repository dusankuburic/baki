package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// F-42: the SSE error payload carries the shared-credential flag only when
// set — the frontend's parser (chatEvent.ts) treats `shared === true` as the
// only signal, so an unshared failure must not carry the key at all (it
// parses a present-but-false key fine, but omission is the wire contract for
// optional facts, like the code when unclassified).
func TestErrorEventData_SharedFlag(t *testing.T) {
	plain := errorEventData(ChatErrProviderAuth, "the API key for this provider is invalid — check it in Settings → AI Accounts", false)
	if _, ok := plain["shared"]; ok {
		t.Errorf("an unshared failure carries shared: %v", plain)
	}
	if plain["code"] != ChatErrProviderAuth || plain["message"] == "" {
		t.Errorf("plain payload = %v, want the code and message kept", plain)
	}
	shared := errorEventData(ChatErrProviderAuth, "the provider rejected your organisation's shared credentials — ask your organisation admin to update the shared Azure AI Foundry configuration", true)
	if shared["shared"] != true {
		t.Errorf("shared payload = %v, want shared:true", shared)
	}
}

// F-42: a client that missed the live error event (reload, late begin) gets
// the shared flag from the stored state too — ResumeStream answers with it,
// BeginStream hands the snapshot back, and the wire form omits it when unset,
// like errorCode.
func TestResumeAndBegin_CarryTheErrorSharedFlag(t *testing.T) {
	svc := &ChatService{}
	ctl := &streamCtl{ownerID: "user-1", streamID: "s1", started: make(chan struct{})}
	ctl.setFailure(ChatErrProviderNotConfigured, "your organisation's shared provider configuration is invalid — ask your organisation admin to update the shared Azure AI Foundry configuration", true)
	svc.activeStreams.Store("s1", ctl)

	res, err := svc.ResumeStream(context.Background(), "s1", 0)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}
	if !res.ErrorShared || res.ErrorCode != ChatErrProviderNotConfigured {
		t.Errorf("resume: ErrorShared=%v ErrorCode=%q, want the stored flag and code", res.ErrorShared, res.ErrorCode)
	}
	if b, _ := json.Marshal(res); !strings.Contains(string(b), `"errorShared":true`) {
		t.Errorf("resume json = %s, want errorShared:true", b)
	}
	begin := svc.BeginStream(context.Background(), "s1")
	if begin == nil || !begin.ErrorShared {
		t.Errorf("begin: got %+v, want ErrorShared", begin)
	}

	// Unset stays absent on the wire (the snapshot is the begin/resume and
	// the cross-replica mirror payload).
	own := &streamCtl{ownerID: "user-1", streamID: "s2", started: make(chan struct{})}
	own.setFailure(ChatErrProviderAuth, "the API key for this provider is invalid", false)
	if b, _ := json.Marshal(own.snapshot()); strings.Contains(string(b), "errorShared") {
		t.Errorf("unshared snapshot marshals errorShared: %s", b)
	}
}

// F-42: the org invalid-config failure — the one variant whose Accounts
// action has nothing the member can fix — is classified shared on the org
// credential. (The full three-error matrix is
// TestClassifyFailure_SharedCredentialCopy in chat_org_key_test.go.)
func TestClassifyFailureShared_OrgInvalidConfig(t *testing.T) {
	shared := &streamCtl{}
	shared.sharedCredential.Store(true)
	code, msg, flag := shared.classifyFailureShared(context.Background(), ai.ErrProviderConfigInvalid)
	if code != ChatErrProviderNotConfigured || !flag {
		t.Errorf("org invalid config: got (%q, shared=%v), want %q and the shared flag", code, flag, ChatErrProviderNotConfigured)
	}
	if !strings.Contains(msg, "ask your organisation admin") {
		t.Errorf("org invalid config: message %q lacks the admin copy", msg)
	}
}

// F-42 wiring: a provider failure on the organisation's shared credential
// reaches the SSE error event with the shared flag (and the org-admin copy),
// while the same provider failure on the member's own credential emits no
// shared key at all — the parser treats presence as the only signal.
func TestRunToolLoop_SharedCredentialFailureCarriesTheFlag(t *testing.T) {
	svc := &ChatService{
		analysisCache: &AnalysisService{},
		backend:       &testutil.FakeBackend{},
		settings:      &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100}}},
	}
	run := func(shared bool) []capturedEvent {
		ctl := &streamCtl{}
		if shared {
			ctl.sharedCredential.Store(true)
		}
		emit, events := captureEvents()
		svc.runToolLoop(context.Background(), &testutil.FakeProvider{Err: ai.ErrProviderConfigInvalid}, ai.Request{}, "user-1",
			&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })
		return events()
	}

	shared := run(true)
	if len(shared) != 1 || shared[0].typ != "error" {
		t.Fatalf("shared credential: expected one error event, got %+v", shared)
	}
	if shared[0].data["shared"] != true {
		t.Errorf("shared credential: error event = %+v, want shared:true", shared[0].data)
	}
	if shared[0].data["code"] != ChatErrProviderNotConfigured {
		t.Errorf("shared credential: code = %v, want %s", shared[0].data["code"], ChatErrProviderNotConfigured)
	}

	own := run(false)
	if len(own) != 1 || own[0].typ != "error" {
		t.Fatalf("own credential: expected one error event, got %+v", own)
	}
	if _, ok := own[0].data["shared"]; ok {
		t.Errorf("own credential: error event = %+v, must not carry the shared key", own[0].data)
	}
	if own[0].data["code"] != ChatErrProviderNotConfigured {
		t.Errorf("own credential: code = %v, want %s (only the copy differs)", own[0].data["code"], ChatErrProviderNotConfigured)
	}
}
