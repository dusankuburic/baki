package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/hmacsig"
)

// signedCI is one signed webhook call as a runner sent it, so a test can send the very same bytes again (a captured
// call replayed inside the timestamp window).
type signedCI struct {
	body                 []byte
	keyID, ts, signature string
}

func signCI(t *testing.T, key, keyID string, body map[string]string) signedCI {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	return signedCI{body: raw, keyID: keyID, ts: strconv.FormatInt(ts, 10), signature: hmacsig.Sign([]byte(key), ts, raw)}
}

func (c signedCI) request() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(c.body))
	req.Header.Set(hmacsig.SignatureHeader, c.signature)
	req.Header.Set(hmacsig.TimestampHeader, c.ts)
	if c.keyID != "" {
		req.Header.Set(hmacsig.KeyIDHeader, c.keyID)
	}
	return req
}

// ciDeliveries reads pad_ci_webhook_deliveries_total{delivery=outcome}.
func ciDeliveries(t *testing.T, outcome string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range fams {
		if mf.GetName() != "pad_ci_webhook_deliveries_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "delivery" && lp.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func wantReplayed(t *testing.T, what string, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"CI_REPLAYED"`) {
		t.Errorf("%s: %d %s, want 409 CI_REPLAYED", what, rr.Code, rr.Body.String())
	}
}

// A delivery id the signature covers makes a captured call good once: the same bytes sent again inside the
// timestamp window are a 409 that runs nothing, as is the id re-signed with a fresh timestamp. The record is per
// key: another credential, or the deployment secret, may use the same id; the old-client fallback authenticates
// the same credential and so shares its record.
func TestCIWebhook_DeliveryIDStopsAReplay(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	other := f.mint(t, f.org, "other")
	runs := captureAuditEvents(t, 32)
	const delivery = "0123456789abcdef0123456789abcdef"
	body := map[string]string{"flowId": f.orgFlow, "deliveryId": delivery}
	replayedBefore := ciDeliveries(t, "replayed")

	captured := signCI(t, cred.Credential, cred.ID, body)
	checkStatus(t, serveCI(f.rt, captured.request()), http.StatusOK)
	if ev := nextAuditEvent(t, runs); ev.Action != "ci.webhook_run" {
		t.Fatalf("audit row %s", ev.Action)
	}
	wantReplayed(t, "the captured call", serveCI(f.rt, captured.request()))
	wantReplayed(t, "re-signed", serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, body)))
	wantReplayed(t, "through the old-client fallback", serveCI(f.rt, ciCall(t, cred.Credential, "", body)))
	if n := len(runs); n != 0 {
		t.Errorf("a replay ran: %d audit rows", n)
	}
	if got := ciDeliveries(t, "replayed") - replayedBefore; got != 3 {
		t.Errorf("pad_ci_webhook_deliveries_total{delivery=replayed} grew by %v, want 3", got)
	}

	checkStatus(t, serveCI(f.rt, ciCall(t, other.Credential, other.ID, body)), http.StatusOK)
	setCIGlobalSecret(t, f.rt, "topsecret")
	checkStatus(t, serveCI(f.rt, ciCall(t, "topsecret", "", body)), http.StatusOK)
	wantReplayed(t, "the deployment secret again", serveCI(f.rt, ciCall(t, "topsecret", "", body)))
}

// A call without a delivery id (a bakicli release before it) is still accepted every time, and counted.
func TestCIWebhook_NoDeliveryIDIsAcceptedAndCounted(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	before := ciDeliveries(t, "missing")
	call := signCI(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow})
	for range 2 {
		checkStatus(t, serveCI(f.rt, call.request()), http.StatusOK)
	}
	if got := ciDeliveries(t, "missing") - before; got != 2 {
		t.Errorf("pad_ci_webhook_deliveries_total{delivery=missing} grew by %v, want 2", got)
	}
}

// A malformed delivery id is the caller's error, after authentication; nothing runs.
func TestCIWebhook_MalformedDeliveryIDIs400(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	runs := captureAuditEvents(t, 8)
	for _, id := range []string{"short", strings.Repeat("a", 129), "0123456789abcdef 0123456789abcdef", "0123456789abcdef/0123456789abcdef", "ü123456789abcdef0123456789abcdef"} {
		rr := serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow, "deliveryId": id}))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "deliveryId") {
			t.Errorf("deliveryId %q: %d %s, want 400", id, rr.Code, rr.Body.String())
		}
	}
	if n := len(runs); n != 0 {
		t.Errorf("%d runs for malformed delivery ids", n)
	}
	// A bad signature never reaches the record: the same id is still good afterwards.
	const delivery = "fedcba9876543210fedcba9876543210"
	if rr := serveCI(f.rt, ciCall(t, cred.Credential+"x", cred.ID, map[string]string{"flowId": f.orgFlow, "deliveryId": delivery})); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rr.Code)
	}
	checkStatus(t, serveCI(f.rt, ciCall(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow, "deliveryId": delivery})), http.StatusOK)
}

// Of concurrent calls with one delivery id exactly one runs.
func TestCIWebhook_ConcurrentReplaysRunOnce(t *testing.T) {
	f := newCIOrgFixture(t)
	cred := f.mint(t, f.org, "pipeline")
	call := signCI(t, cred.Credential, cred.ID, map[string]string{"flowId": f.orgFlow, "deliveryId": "concurrent-0123456789abcdef"})
	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- serveCI(f.rt, call.request()).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusOK] != 1 || got[http.StatusConflict] != n-1 {
		t.Errorf("statuses %v, want one 200 and %d 409", got, n-1)
	}
}
