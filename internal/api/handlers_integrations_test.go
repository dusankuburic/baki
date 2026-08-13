package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"pad-core/models"
)

// signBody computes the X-Baki-Signature value the CI runner would send (legacy
// form: no timestamp, body-only).
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// signBodyWithTimestamp computes the X-Baki-Signature including a timestamp in
// the signed payload (replay-protected form). The timestamp must also be sent
// in the X-Baki-Timestamp header.
func signBodyWithTimestamp(secret string, body []byte, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// The CI webhook is in publicRoutes, so it bypasses jwtAuth entirely — these
// tests hit it with no Authorization header. Auth is the HMAC signature alone.

func TestCIWebhook_Unconfigured_Returns503(t *testing.T) {
	rt := newJWTTestRouter(t)
	// Default test router wires ciSecret="" → endpoint disabled.

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when CI webhook unconfigured, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_MissingSignature_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	rt.handlers.Analysis.ciSecret = "topsecret"

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing signature, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_InvalidSignature_Returns401(t *testing.T) {
	rt := newJWTTestRouter(t)
	rt.handlers.Analysis.ciSecret = "topsecret"

	body, _ := json.Marshal(map[string]string{"flowId": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	req.Header.Set("X-Baki-Signature", "sha256=deadbeef")
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid signature, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCIWebhook_ValidSignature_MissingFlowID_Returns400(t *testing.T) {
	rt := newJWTTestRouter(t)
	rt.handlers.Analysis.ciSecret = "topsecret"

	body, _ := json.Marshal(map[string]string{}) // no flowId
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(body))
	req.Header.Set("X-Baki-Signature", signBody("topsecret", body))
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing flowId, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestVerifySignature_MatchesSignedBody(t *testing.T) {
	secret := "k"
	body := []byte(`{"flowId":"abc"}`)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Baki-Signature", signBody(secret, body))
	ok, err := verifySignature(req, secret)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if !ok {
		t.Error("expected signature to verify")
	}
	// Body must still be readable after verification (restored for the handler).
	buf := make([]byte, 64)
	n, _ := req.Body.Read(buf)
	if n == 0 {
		t.Error("body was not restored after signature verification")
	}
}

func TestVerifySignature_RejectsTamperedBody(t *testing.T) {
	secret := "k"
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"flowId":"tampered"}`)))
	req.Header.Set("X-Baki-Signature", signBody(secret, []byte(`{"flowId":"original"}`)))
	ok, err := verifySignature(req, secret)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if ok {
		t.Error("expected signature verification to fail for a tampered body")
	}
}

// Replay protection: a fresh X-Baki-Timestamp folded into the signed payload is
// accepted.
func TestVerifySignature_AcceptsFreshTimestamp(t *testing.T) {
	secret := "k"
	body := []byte(`{"flowId":"abc"}`)
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Baki-Signature", signBodyWithTimestamp(secret, body, ts))
	ok, err := verifySignature(req, secret)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if !ok {
		t.Error("expected signature with a fresh timestamp to verify")
	}
}

// A timestamp outside the ±5min skew window is rejected even with a matching
// signature — so a captured request can't be replayed later.
func TestVerifySignature_RejectsStaleTimestamp(t *testing.T) {
	secret := "k"
	body := []byte(`{"flowId":"abc"}`)
	stale := time.Now().Add(-(ciWebhookMaxSkew + time.Minute)).Unix()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(stale, 10))
	req.Header.Set("X-Baki-Signature", signBodyWithTimestamp(secret, body, stale))
	ok, err := verifySignature(req, secret)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if ok {
		t.Error("expected signature with a stale timestamp to be rejected")
	}
}

// Refreshing just the timestamp on a captured request breaks the signature
// (the timestamp is part of the signed payload).
func TestVerifySignature_RejectsTamperedTimestamp(t *testing.T) {
	secret := "k"
	body := []byte(`{"flowId":"abc"}`)
	old := time.Now().Add(-(ciWebhookMaxSkew + time.Minute)).Unix()
	fresh := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	// Signature computed over the OLD timestamp, header carries the FRESH one.
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(fresh, 10))
	req.Header.Set("X-Baki-Signature", signBodyWithTimestamp(secret, body, old))
	ok, err := verifySignature(req, secret)
	if err != nil {
		t.Fatalf("verifySignature: %v", err)
	}
	if ok {
		t.Error("expected signature with a tampered timestamp to be rejected")
	}
}

func TestGatePasses(t *testing.T) {
	stats := func(e, w, i int) models.AnalysisStats {
		return models.AnalysisStats{Errors: e, Warnings: w, Info: i}
	}
	cases := []struct {
		name  string
		stats models.AnalysisStats
		gate  string
		want  bool
	}{
		{"none always passes", stats(5, 5, 5), "none", true},
		{"error gate: zero errors passes", stats(0, 5, 5), "error", true},
		{"error gate: errors fail", stats(1, 0, 0), "error", false},
		{"warning gate: warnings fail", stats(0, 1, 0), "warning", false},
		{"warning gate: errors also fail", stats(1, 0, 0), "warning", false},
		{"info gate: info fails", stats(0, 0, 1), "info", false},
		{"info gate: clean passes", stats(0, 0, 0), "info", true},
		{"unknown gate defaults to error behavior", stats(1, 0, 0), "bogus", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := &models.AnalysisReport{Stats: c.stats}
			if got := gatePasses(rep, c.gate); got != c.want {
				t.Errorf("gatePasses(gate=%q) = %v, want %v", c.gate, got, c.want)
			}
		})
	}
}
