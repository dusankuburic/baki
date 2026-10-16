package main

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/platform/hmacsig"
)

// ciWebhookPath is the server route `bakicli report` targets.
const ciWebhookPath = "/api/integrations/ci"

// runReport implements `bakicli report`: ask a PAD Analyzer server to analyse
// a STORED flow and record the run on its dashboard through POST
// /api/integrations/ci, signed the way the server verifies it (internal/platform/hmacsig:
// HMAC-SHA256 over "<unix-seconds>." + body, hex, "sha256=" prefix, timestamp
// echoed in X-Baki-Timestamp; rejected if more than five minutes off the
// server clock). The key is an organisation CI credential (baki_ci_<keyId>_<secret>,
// whose key id goes in X-Baki-Key-Id) or the server's PAD_CI_WEBHOOK_SECRET.
// Instead of a key, -token sends a personal API token with the ci scope as
// Authorization: Bearer, for flows without an organisation.
//
// The endpoint does not ingest a runner's report — the server re-analyses the
// flow it holds, so dashboards never trust numbers a pipeline hands them — so
// the body is just {flowId, failOn, format, deliveryId}. deliveryId is random
// per invocation and covered by the signature: the server runs a key's id
// once, so a captured call cannot be replayed inside the timestamp window. A
// local report.json may supply the flow ID.
func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "PAD Analyzer server base URL (https://baki.example.com) or the full "+ciWebhookPath+" URL")
	secret := fs.String("secret", "", "signing key: an org CI credential (Settings → Organisation → CI credentials) or the server's PAD_CI_WEBHOOK_SECRET (default: $PAD_CI_WEBHOOK_SECRET, so it need not appear on the command line)")
	token := fs.String("token", "", "instead of -secret: a personal API token with the ci scope (Settings → API Tokens), for flows without an organisation; sent as Authorization: Bearer, so the endpoint must be https or loopback (default: $PAD_CI_TOKEN)")
	flowID := fs.String("flow-id", "", "stored flow ID to analyse (default: the flowId of the report.json argument)")
	failOn := fs.String("fail-on", "error", "severity at which the server reports passed=false: error, warning, info, none")
	format := fs.String("format", "summary", "response shape: summary, report (adds findings), sarif")
	timeout := fs.Duration("timeout", 60*time.Second, "HTTP timeout")
	quiet := fs.Bool("quiet", false, "print only the server response")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli report -endpoint URL [-secret SECRET | -token TOKEN] [-flow-id ID] [-fail-on error] [report.json]")
		fmt.Fprintln(os.Stderr, "  record an analysis run on a PAD Analyzer server (POST "+ciWebhookPath+", HMAC-signed or with an API token)")
		fmt.Fprintln(os.Stderr, "  exit 0 when the server's gate passed, 1 when it failed, 2 on a request error")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() > 1 {
		fs.Usage()
		os.Exit(2)
	}
	caller, err := resolveCICaller(fs, *secret, *token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
		os.Exit(2)
	}
	if *endpoint == "" {
		fmt.Fprintln(os.Stderr, "bakicli report: -endpoint is required")
		os.Exit(2)
	}
	url := ciWebhookURL(*endpoint)
	if caller.token != "" && plainHTTPRemote(url) {
		fmt.Fprintln(os.Stderr, "bakicli report: refusing to send an API token over plain HTTP: use an https endpoint (or a loopback address)")
		os.Exit(2)
	}
	// The HMAC credential never travels, so a warning suffices — but the VERDICT
	// comes back over the same plaintext link, and a network attacker can forge
	// the "passed" summary the gate exits 0 on. Loopback stays silent (local dev).
	if caller.token == "" && caller.secret != "" && plainHTTPRemote(url) {
		fmt.Fprintln(os.Stderr, "bakicli report: warning: plain-HTTP endpoint — the signed report is safe in transit, but the returned verdict is not authenticated and a network attacker could forge a pass")
	}
	if *flowID == "" && fs.NArg() == 1 {
		rep, err := readReport(fs.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
			os.Exit(2)
		}
		*flowID = rep.FlowID
	}
	if *flowID == "" {
		fmt.Fprintln(os.Stderr, "bakicli report: a flow ID is required (-flow-id, or a report.json whose flowId is set)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	resp, err := postCIReport(ctx, url, caller, map[string]string{"flowId": *flowID, "failOn": *failOn, "format": *format}, *quiet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		fmt.Fprintf(os.Stderr, "bakicli report: server returned %s: %s\n", resp.Status, strings.TrimSpace(string(respBody)))
		os.Exit(2)
	}

	// SARIF is an export document, streamed as it came (it has no size
	// bound: about 1.2 KB per finding). The server normalises the format and
	// answers every other value with the verdict, so every other value gates
	// (a "Summary" or "json" must not exit 0 on a failure).
	if strings.ToLower(strings.TrimSpace(*format)) == "sarif" {
		if err := copyEndingInNewline(os.Stdout, resp.Body); err != nil {
			fmt.Fprintf(os.Stderr, "bakicli report: reading the SARIF response: %v\n", err)
			os.Exit(2)
		}
		return
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxVerdictBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: reading the response: %v\n", err)
		os.Exit(2)
	}
	if len(respBody) > maxVerdictBytes {
		fmt.Fprintf(os.Stderr, "bakicli report: the server's verdict is larger than %d bytes\n", maxVerdictBytes)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stdout, "%s\n", bytes.TrimSpace(respBody))
	var verdict struct {
		FlowName string `json:"flowName"`
		Passed   *bool  `json:"passed"`
		Gate     string `json:"gate"`
		Errors   int    `json:"errors"`
		Warnings int    `json:"warnings"`
		Info     int    `json:"info"`
	}
	if err := json.Unmarshal(respBody, &verdict); err != nil || verdict.Passed == nil {
		fmt.Fprintln(os.Stderr, "bakicli report: server response carried no gate verdict")
		os.Exit(2)
	}
	if !*quiet {
		state := "passed"
		if !*verdict.Passed {
			state = "FAILED"
		}
		fmt.Fprintf(os.Stderr, "bakicli report: %s — gate %s (fail-on=%s): %d errors, %d warnings, %d info\n",
			verdict.FlowName, state, verdict.Gate, verdict.Errors, verdict.Warnings, verdict.Info)
	}
	if !*verdict.Passed {
		os.Exit(1)
	}
}

// ciRetries bounds the retries of a rate-limited call. The server limits the webhook per client address, which a
// pipeline matrix or a runner pool behind one NAT address shares; a refused call recorded nothing, so it is resent.
const ciRetries = 4

// ciMaxRetryWait caps one wait between attempts, whatever the server's Retry-After says.
const ciMaxRetryWait = 30 * time.Second

// postCIReport sends the call (signed again for each attempt, the timestamp being part of it) and retries a 429
// after the server's Retry-After, or 1, 2, 4 s without one, plus up to half again at random so the calls of a
// matrix spread out. The -timeout bounds the whole exchange, waits included.
//
// fields is the body without its deliveryId, which is minted here: one per invocation, kept across retries of a
// 429 from the rate limiter (code RATE_LIMITED, or a proxy's 429 that is no API error), which answers before the
// handler records anything, so a retry of a call that never ran is the same call. Any other 429 came from the
// handler, which may have recorded the id: the retry gets a new one, or the server would answer it 409.
func postCIReport(ctx context.Context, url string, caller ciCaller, fields map[string]string, quiet bool) (*http.Response, error) {
	body := maps.Clone(fields)
	for attempt := 0; ; attempt++ {
		if body["deliveryId"] == "" {
			id, err := newDeliveryID()
			if err != nil {
				return nil, err
			}
			body["deliveryId"] = id
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := caller.request(ctx, url, raw, time.Now())
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || attempt == ciRetries {
			return resp, err
		}
		refusal, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if !rateLimiterRefusal(refusal) {
			delete(body, "deliveryId")
		}
		wait := ciRetryWait(resp.Header.Get("Retry-After"), attempt)
		if !quiet {
			fmt.Fprintf(os.Stderr, "bakicli report: rate limited by the server; retrying in %s\n", wait.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("rate limited by the server until the timeout: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// rateLimiterRefusal reports whether a 429's body is the rate limiter's refusal: the API's RATE_LIMITED code, or
// no API error at all (a proxy in front of the server).
func rateLimiterRefusal(body []byte) bool {
	var apiErr struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &apiErr) != nil || apiErr.Code == "" {
		return true
	}
	return apiErr.Code == "RATE_LIMITED"
}

// newDeliveryID returns a call's delivery id: 128 random bits, hex.
func newDeliveryID() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", fmt.Errorf("delivery id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ciCaller is how `bakicli report` authenticates: a signing key (secret) or a personal API token (token), never
// both.
type ciCaller struct{ secret, token string }

// resolveCICaller picks the credential from -secret and -token, each defaulting to its environment variable unless
// the other flag is given: the two flags together, or both variables without either flag, are an error, as is no
// credential at all.
func resolveCICaller(fs *flag.FlagSet, secret, token string) (ciCaller, error) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["secret"] && set["token"] {
		return ciCaller{}, fmt.Errorf("-secret and -token are mutually exclusive")
	}
	if secret == "" && !set["token"] {
		secret = os.Getenv("PAD_CI_WEBHOOK_SECRET")
	}
	if token == "" && !set["secret"] {
		token = os.Getenv("PAD_CI_TOKEN")
	}
	switch {
	case secret != "" && token != "":
		return ciCaller{}, fmt.Errorf("both $PAD_CI_WEBHOOK_SECRET and $PAD_CI_TOKEN are set: pass -secret or -token to choose")
	case secret == "" && token == "":
		return ciCaller{}, fmt.Errorf("-secret (or $PAD_CI_WEBHOOK_SECRET) or -token (or $PAD_CI_TOKEN) is required")
	case token != "" && !strings.HasPrefix(token, apiTokenPrefix):
		return ciCaller{}, fmt.Errorf("-token takes a personal API token (%s…); an organisation CI credential or the server's secret goes in -secret", apiTokenPrefix)
	}
	return ciCaller{secret: secret, token: token}, nil
}

// apiTokenPrefix starts a personal API token (internal/auth.APITokenPrefix).
const apiTokenPrefix = "pad_pat_"

// request builds one attempt's POST: signed with the key, or carrying the token.
func (c ciCaller) request(ctx context.Context, url string, body []byte, now time.Time) (*http.Request, error) {
	if c.token == "" {
		return newSignedCIRequest(ctx, url, c.secret, body, now)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "bakicli/"+version)
	return req, nil
}

// plainHTTPRemote reports whether rawURL is plain http to a host other than a loopback address: a bearer token
// sent there crosses the network readable.
func plainHTTPRemote(rawURL string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return err != nil
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// ciRetryWait is the wait before retry attempt+1: the Retry-After seconds when the server sent them, otherwise
// 1 s doubled per attempt; plus up to half of it at random, and at most ciMaxRetryWait.
func ciRetryWait(retryAfter string, attempt int) time.Duration {
	wait := time.Second << attempt
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		wait = time.Duration(s) * time.Second
	}
	if wait > 0 {
		wait += rand.N(wait/2 + 1) // #nosec G404 -- retry jitter, not a secret
	}
	return min(wait, ciMaxRetryWait)
}

// maxVerdictBytes bounds the gate verdict `bakicli report` reads into memory.
const maxVerdictBytes = 16 << 20

// copyEndingInNewline copies r to w and ends the output with a newline when r
// did not.
func copyEndingInNewline(w io.Writer, r io.Reader) error {
	buf := make([]byte, 64<<10)
	var last byte = '\n'
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			last = buf[n-1]
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if last != '\n' {
		_, err := io.WriteString(w, "\n")
		return err
	}
	return nil
}

// ciWebhookURL accepts either the server origin or the full route.
func ciWebhookURL(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(e, ciWebhookPath) {
		return e
	}
	return e + ciWebhookPath
}

// signCIRequest computes the X-Baki-Signature value for body sent at unix-second
// timestamp ts: "sha256=" + hex(HMAC-SHA256(secret, "<ts>." + body)), the
// canonical string the server's CI service verifies; the same ts must travel in
// X-Baki-Timestamp. The key is the whole secret, an org credential included.
func signCIRequest(secret string, ts int64, body []byte) string {
	return hmacsig.Sign([]byte(secret), ts, body)
}

// ciCredentialPrefix starts an organisation CI credential, baki_ci_<keyId>_<secret>.
const ciCredentialPrefix = "baki_ci_"

// ciKeyID returns the key id of an organisation CI credential, "" for any other
// secret (the server's PAD_CI_WEBHOOK_SECRET). Key ids hold no underscore.
func ciKeyID(secret string) string {
	rest, ok := strings.CutPrefix(secret, ciCredentialPrefix)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "_")
	if !ok {
		return ""
	}
	return id
}

// newSignedCIRequest builds the POST with the signature headers set, and the
// key id of an organisation CI credential.
func newSignedCIRequest(ctx context.Context, url, secret string, body []byte, now time.Time) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hmacsig.TimestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(hmacsig.SignatureHeader, signCIRequest(secret, ts, body))
	if id := ciKeyID(secret); id != "" {
		req.Header.Set(hmacsig.KeyIDHeader, id)
	}
	req.Header.Set("User-Agent", "bakicli/"+version)
	return req, nil
}
