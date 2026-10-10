package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"pad-analyzer/internal/platform/hmacsig"
)

// ciCallSeen is one call a test server received.
type ciCallSeen struct {
	deliveryID, authorization, signature, timestamp string
	signedWith                                      func(key string) bool
}

// recordingCIServer answers each call with answers[i] (status, body), the last one again past the end, and keeps
// what it saw.
func recordingCIServer(t *testing.T, answers ...[2]string) (*httptest.Server, func() []ciCallSeen) {
	t.Helper()
	var mu sync.Mutex
	var seen []ciCallSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var fields map[string]string
		_ = json.Unmarshal(body, &fields)
		ts := r.Header.Get(hmacsig.TimestampHeader)
		sig := r.Header.Get(hmacsig.SignatureHeader)
		mu.Lock()
		n := len(seen)
		seen = append(seen, ciCallSeen{deliveryID: fields["deliveryId"], authorization: r.Header.Get("Authorization"), signature: sig, timestamp: ts,
			signedWith: func(key string) bool {
				unix, err := strconv.ParseInt(ts, 10, 64)
				return err == nil && hmacsig.Verify([]byte(key), unix, body, sig)
			}})
		mu.Unlock()
		a := answers[min(n, len(answers)-1)]
		status, _ := strconv.Atoi(a[0])
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, a[1])
	}))
	t.Cleanup(srv.Close)
	return srv, func() []ciCallSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]ciCallSeen(nil), seen...)
	}
}

var (
	passed        = [2]string{"200", `{"flowId":"f","passed":true,"gate":"error"}`}
	limited       = [2]string{"429", `{"code":"RATE_LIMITED","message":"rate limit exceeded"}`}
	proxyLimited  = [2]string{"429", `Too Many Requests`}
	handlerBusy   = [2]string{"429", `{"code":"CI_BUSY","message":"try again"}`}
	hexDeliveryID = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Every invocation sends a fresh 128-bit delivery id in the body, which the signature covers.
func TestCLI_ReportSendsADeliveryID(t *testing.T) {
	srv, seen := recordingCIServer(t, passed)
	for range 2 {
		if _, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "topsecret", "-flow-id", "f"); code != 0 {
			t.Fatalf("exit %d (%s)", code, stderr)
		}
	}
	calls := seen()
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	for _, c := range calls {
		if !hexDeliveryID.MatchString(c.deliveryID) || !c.signedWith("topsecret") {
			t.Errorf("deliveryId %q (signed %v); want 32 hex characters under the signature", c.deliveryID, c.signedWith("topsecret"))
		}
	}
	if calls[0].deliveryID == calls[1].deliveryID {
		t.Error("two invocations sent the same delivery id")
	}
}

// A retry after the rate limiter's 429 (or a proxy's) is the same call, which never ran: same delivery id, signed
// again. A 429 from the handler may have recorded the id, so the retry gets a new one.
func TestCLI_ReportRetryKeepsTheDeliveryIDOfACallThatNeverRan(t *testing.T) {
	for name, c := range map[string]struct {
		refusal [2]string
		same    bool
	}{
		"rate limiter": {limited, true},
		"proxy":        {proxyLimited, true},
		"handler":      {handlerBusy, false},
	} {
		srv, seen := recordingCIServer(t, c.refusal, passed)
		if _, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "topsecret", "-flow-id", "f", "-quiet"); code != 0 {
			t.Fatalf("%s: exit %d (%s)", name, code, stderr)
		}
		calls := seen()
		if len(calls) != 2 || !calls[1].signedWith("topsecret") {
			t.Fatalf("%s: %d calls; want 2, each signed", name, len(calls))
		}
		if got := calls[0].deliveryID == calls[1].deliveryID; got != c.same || calls[1].deliveryID == "" {
			t.Errorf("%s: the retry's delivery id %q after %q; want the same: %v", name, calls[1].deliveryID, calls[0].deliveryID, c.same)
		}
	}
}

// -token (or $PAD_CI_TOKEN) sends the API token as a bearer instead of a signature, with a delivery id; it excludes
// -secret, refuses anything but an API token, and never goes out over plain HTTP to a remote host.
func TestCLI_ReportWithAToken(t *testing.T) {
	const token = "pad_pat_0123456789abcdef"
	srv, seen := recordingCIServer(t, passed)
	clean := []string{"PAD_CI_WEBHOOK_SECRET=", "PAD_CI_TOKEN="}
	if _, stderr, code := runBakicliIn(t, ".", clean, "report", "-endpoint", srv.URL, "-token", token, "-flow-id", "f"); code != 0 {
		t.Fatalf("-token: exit %d (%s)", code, stderr)
	}
	// From the environment; an explicit -token wins over the secret's variable.
	if _, stderr, code := runBakicliIn(t, ".", []string{"PAD_CI_WEBHOOK_SECRET=", "PAD_CI_TOKEN=" + token}, "report", "-endpoint", srv.URL, "-flow-id", "f"); code != 0 {
		t.Fatalf("$PAD_CI_TOKEN: exit %d (%s)", code, stderr)
	}
	if _, stderr, code := runBakicliIn(t, ".", []string{"PAD_CI_WEBHOOK_SECRET=topsecret", "PAD_CI_TOKEN="}, "report", "-endpoint", srv.URL, "-token", token, "-flow-id", "f"); code != 0 {
		t.Fatalf("-token beside $PAD_CI_WEBHOOK_SECRET: exit %d (%s)", code, stderr)
	}
	calls := seen()
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	for i, c := range calls {
		if c.authorization != "Bearer "+token || c.signature != "" || c.timestamp != "" || !hexDeliveryID.MatchString(c.deliveryID) {
			t.Errorf("call %d: authorization %q, signature %q, timestamp %q, deliveryId %q", i, c.authorization, c.signature, c.timestamp, c.deliveryID)
		}
	}

	for name, run := range map[string]struct {
		env  []string
		args []string
	}{
		"-token with -secret":  {clean, []string{"-token", token, "-secret", "topsecret"}},
		"both variables":       {[]string{"PAD_CI_WEBHOOK_SECRET=topsecret", "PAD_CI_TOKEN=" + token}, nil},
		"not an API token":     {clean, []string{"-token", "baki_ci_abcdefghijklmnop_secret"}},
		"plain HTTP to remote": {clean, []string{"-token", token, "-endpoint", "http://baki.example.com"}},
		"no credential":        {clean, nil},
	} {
		args := append([]string{"report", "-endpoint", srv.URL, "-flow-id", "f"}, run.args...)
		if _, _, code := runBakicliIn(t, ".", run.env, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if n := len(seen()); n != 3 {
		t.Errorf("a refused invocation reached the server (%d calls)", n)
	}
}

func TestPlainHTTPRemote(t *testing.T) {
	for url, want := range map[string]bool{
		"https://baki.example.com/api/integrations/ci": false,
		"http://baki.example.com/api/integrations/ci":  true,
		"HTTP://baki.example.com":                      true,
		"http://10.0.0.5:8080":                         true,
		"http://127.0.0.1:8080/api/integrations/ci":    false,
		"http://[::1]:8080":                            false,
		"http://localhost:8080":                        false,
		"http://localhost.example.com":                 true,
		"://bad":                                       true,
	} {
		if got := plainHTTPRemote(url); got != want {
			t.Errorf("plainHTTPRemote(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestRateLimiterRefusal(t *testing.T) {
	for body, want := range map[string]bool{
		`{"code":"RATE_LIMITED","message":"rate limit exceeded"}`: true,
		`Too Many Requests`:          true,
		`{"message":"slow down"}`:    true,
		`{"code":"CI_BUSY"}`:         false,
		`{"code":"BUDGET_EXCEEDED"}`: false,
	} {
		if got := rateLimiterRefusal([]byte(body)); got != want {
			t.Errorf("rateLimiterRefusal(%s) = %v, want %v", body, got, want)
		}
	}
}
