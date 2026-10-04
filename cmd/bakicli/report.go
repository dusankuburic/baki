package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ciWebhookPath is the server route `bakicli report` targets.
const ciWebhookPath = "/api/integrations/ci"

// runReport implements `bakicli report`: ask a PAD Analyzer server to analyse
// a STORED flow and record the run on its dashboard through POST
// /api/integrations/ci, signed exactly the way the server's verifySignature
// checks it (HMAC-SHA256 over "<unix-seconds>." + body, hex, "sha256=" prefix,
// timestamp echoed in X-Baki-Timestamp; rejected if more than five minutes off
// the server clock).
//
// The endpoint does not ingest a runner's report — the server re-analyses the
// flow it holds, so dashboards never trust numbers a pipeline hands them — so
// the body is just {flowId, failOn, format}. A local report.json may supply
// the flow ID.
func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "PAD Analyzer server base URL (https://baki.example.com) or the full "+ciWebhookPath+" URL")
	secret := fs.String("secret", "", "HMAC key matching the server's PAD_CI_WEBHOOK_SECRET (default: $PAD_CI_WEBHOOK_SECRET, so it need not appear on the command line)")
	flowID := fs.String("flow-id", "", "stored flow ID to analyse (default: the flowId of the report.json argument)")
	failOn := fs.String("fail-on", "error", "severity at which the server reports passed=false: error, warning, info, none")
	format := fs.String("format", "summary", "response shape: summary, report (adds findings), sarif")
	timeout := fs.Duration("timeout", 60*time.Second, "HTTP timeout")
	quiet := fs.Bool("quiet", false, "print only the server response")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bakicli report -endpoint URL [-secret SECRET] [-flow-id ID] [-fail-on error] [report.json]")
		fmt.Fprintln(os.Stderr, "  record an analysis run on a PAD Analyzer server (POST "+ciWebhookPath+", HMAC-signed)")
		fmt.Fprintln(os.Stderr, "  exit 0 when the server's gate passed, 1 when it failed, 2 on a request error")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() > 1 {
		fs.Usage()
		os.Exit(2)
	}
	if *secret == "" {
		*secret = os.Getenv("PAD_CI_WEBHOOK_SECRET")
	}
	if *endpoint == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "bakicli report: -endpoint and -secret (or $PAD_CI_WEBHOOK_SECRET) are required")
		os.Exit(2)
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

	body, err := json.Marshal(map[string]string{"flowId": *flowID, "failOn": *failOn, "format": *format})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := newSignedCIRequest(ctx, ciWebhookURL(*endpoint), *secret, body, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
		os.Exit(2)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bakicli report: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "bakicli report: server returned %s: %s\n", resp.Status, strings.TrimSpace(string(respBody)))
		os.Exit(2)
	}
	fmt.Fprintf(os.Stdout, "%s\n", bytes.TrimSpace(respBody))

	if *format == "sarif" {
		return
	}
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

// ciWebhookURL accepts either the server origin or the full route.
func ciWebhookURL(endpoint string) string {
	e := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(e, ciWebhookPath) {
		return e
	}
	return e + ciWebhookPath
}

// signCIRequest computes the X-Baki-Signature value for body sent at unix-second
// timestamp ts: "sha256=" + hex(HMAC-SHA256(secret, "<ts>." + body)). This is
// the canonical string internal/api.verifySignature recomputes; the same ts
// must travel in X-Baki-Timestamp.
func signCIRequest(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newSignedCIRequest builds the POST with both signature headers set.
func newSignedCIRequest(ctx context.Context, url, secret string, body []byte, now time.Time) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ts := now.Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Baki-Signature", signCIRequest(secret, ts, body))
	req.Header.Set("User-Agent", "bakicli/"+version)
	return req, nil
}
