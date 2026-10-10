package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"
	"go.uber.org/fx"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// stopTestDrainDelay is the child's PAD_SHUTDOWN_DRAIN_DELAY: long enough for its probes to answer within the pause.
const stopTestDrainDelay = 3 * time.Second

// TestApp_StopEndsChatStreamsBeforeSSECloses is R1 through the real graph and its real stop hooks (ADR 0024). A chat
// stream answering over an open event stream when the app stops gets its terminal SERVER_RESTARTING event before the
// event stream closes; the stop used to close the event streams first, so the client never heard why its answer
// stopped. During the drain pause, before the streams end, the readiness probe already answers 503, liveness stays
// 200, and a new chat stream is refused with 503 SERVER_RESTARTING and the wait in its details.
//
// A desktop boot (no database needed) with a drain pause, in a child process as TestApp_StopOrder's boots are: the
// stop drains every lifecycle.Bounded runner of the process, and the boot installs the process-wide logger.
func TestApp_StopEndsChatStreamsBeforeSSECloses(t *testing.T) {
	home := t.TempDir()
	reportPath := filepath.Join(home, "report.json")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStopSequenceChild$", "-test.count=1")
	// PAD_MODE makes the configuration read the environment (a desktop boot without it takes the defaults alone).
	cmd.Env = hermeticEnv(home, "PAD_MODE=local", "PAD_SHUTDOWN_DRAIN_DELAY="+stopTestDrainDelay.String(), stopSequenceReportEnv+"="+reportPath)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child boot: %v\nstderr:\n%s", err, stderr.String())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("child boot report: %v\nstderr:\n%s", err, stderr.String())
	}
	var rep stopSequenceReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("child boot report: %v", err)
	}
	for _, e := range rep.Errors {
		t.Errorf("child: %s", e)
	}
	if t.Failed() {
		t.FailNow()
	}

	// The drain pause: out of rotation at once, alive, and taking no new stream.
	if rep.Readyz.Status != http.StatusServiceUnavailable || !strings.Contains(rep.Readyz.Body, `"status":"draining"`) {
		t.Errorf("/readyz during the drain pause = %d %q, want 503 {\"status\":\"draining\"}", rep.Readyz.Status, rep.Readyz.Body)
	}
	if rep.Healthz.Status != http.StatusOK {
		t.Errorf("/healthz during the drain pause = %d %q, want 200", rep.Healthz.Status, rep.Healthz.Body)
	}
	var refusal struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal([]byte(rep.NewStream.Body), &refusal)
	if rep.NewStream.Status != http.StatusServiceUnavailable || refusal.Code != "SERVER_RESTARTING" ||
		refusal.Details["retryAfterSeconds"] != float64(2) || rep.NewStream.RetryAfter != "2" {
		t.Errorf("POST /api/chat/stream during the drain pause = %d Retry-After %q %s, want 503, Retry-After 2, code SERVER_RESTARTING and details.retryAfterSeconds 2",
			rep.NewStream.Status, rep.NewStream.RetryAfter, rep.NewStream.Body)
	}

	// The running stream heard why it ended, over the connection that was still open.
	if rep.Terminal == "" {
		t.Fatalf("the event stream closed without the stream's terminal event; frames after the stop began: %q", rep.FramesAfterStop)
	}
	if !strings.Contains(rep.Terminal, `"type":"error"`) || !strings.Contains(rep.Terminal, `"code":"SERVER_RESTARTING"`) {
		t.Errorf("terminal event %s, want an error with code SERVER_RESTARTING", rep.Terminal)
	}
	// In order: the probes answered within the pause, before the streams ended.
	for _, p := range []stopProbe{rep.Readyz, rep.Healthz, rep.NewStream} {
		if p.At >= rep.TerminalAt {
			t.Errorf("%s answered %v after the stop began, the stream's terminal event came at %v: want it within the drain pause, before",
				p.Name, p.At, rep.TerminalAt)
		}
	}
	if rep.TerminalAt < stopTestDrainDelay {
		t.Errorf("the stream ended %v after the stop began, before the %v drain pause was over", rep.TerminalAt, stopTestDrainDelay)
	}
}

// stopSequenceReportEnv names the file the child writes its report to; set only for the child.
const stopSequenceReportEnv = "BAKI_TEST_STOP_SEQUENCE_REPORT"

// stopTestProviderID is the chat provider the child registers: it answers one chunk, then holds the stream until it
// is cancelled.
const stopTestProviderID = "app-stop-test"

// stopSequenceReport is what the child saw, for the parent to judge.
type stopSequenceReport struct {
	Errors []string
	// Readyz, Healthz and NewStream are the answers to the probes sent once the stop had begun.
	Readyz, Healthz, NewStream stopProbe
	// Terminal is the stream's terminal event frame as the event stream delivered it ("" when none came before the
	// event stream closed), TerminalAt when, after the stop began.
	Terminal   string
	TerminalAt time.Duration
	// FramesAfterStop is every event frame the event stream delivered after the stop began.
	FramesAfterStop []string
}

// stopProbe is one request sent during the stop.
type stopProbe struct {
	Name       string
	Status     int
	RetryAfter string
	Body       string
	// At is when the answer came, after the stop began.
	At time.Duration
}

// TestStopSequenceChild is the child half of TestApp_StopEndsChatStreamsBeforeSSECloses.
func TestStopSequenceChild(t *testing.T) {
	reportPath := os.Getenv(stopSequenceReportEnv)
	if reportPath == "" {
		t.Skip("the child half of TestApp_StopEndsChatStreamsBeforeSSECloses")
	}
	keyring.MockInitWithError(errKeyringTouched)
	ai.RegisterProvider(stopTestProviderID, func(string) ai.Provider {
		return untilCancelledProvider{FakeProvider: &testutil.FakeProvider{}}
	})

	rep := runStopSequence(t)
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runStopSequence boots the desktop graph, opens an event stream, starts a chat stream on it, stops the app and
// probes it while it stops.
func runStopSequence(t *testing.T) (rep stopSequenceReport) {
	fail := func(format string, args ...any) stopSequenceReport {
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
		return rep
	}

	// The CONFIG line (port, session key path) is printed on stdout while the graph is built.
	stdout, err := os.CreateTemp("", "stdout-*")
	if err != nil {
		return fail("stdout: %v", err)
	}
	defer func() { _ = stdout.Close() }()
	saved := os.Stdout
	os.Stdout = stdout
	var flows *service.FlowService
	app := fx.New(append(Options(Build{}),
		// The test provider's credential; the OS keychain is never reached.
		fx.Decorate(func(service.KeyStore) service.KeyStore { return stopTestKeys{} }),
		fx.Populate(&flows),
		fx.NopLogger,
	)...)
	os.Stdout = saved
	if err := app.Err(); err != nil {
		return fail("build: %v", err)
	}
	printed, err := os.ReadFile(stdout.Name())
	if err != nil {
		return fail("stdout: %v", err)
	}
	var handshake struct {
		Port      int    `json:"port"`
		TokenPath string `json:"tokenPath"`
	}
	line, _ := strings.CutPrefix(strings.TrimSpace(string(printed)), "CONFIG:")
	if err := json.Unmarshal([]byte(line), &handshake); err != nil || handshake.Port == 0 {
		return fail("CONFIG line %q: %v", printed, err)
	}
	secret, err := os.ReadFile(handshake.TokenPath)
	if err != nil {
		return fail("session key: %v", err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", handshake.Port)
	bearer := "Bearer " + string(secret)

	startCtx, cancelStart := context.WithTimeout(context.Background(), app.StartTimeout())
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		return fail("start: %v", err)
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), app.StopTimeout())
	defer cancelStop()
	stopped := make(chan error, 1)
	stopping := false
	defer func() {
		if !stopping {
			_ = app.Stop(stopCtx)
		}
	}()
	// Desktop chat answers about the flow the user has open.
	flows.DocProvider().SetCurrentDoc(&models.FlowDocument{ID: "f-stop", Name: "stop"})

	sseCtx, cancelSSE := context.WithTimeout(context.Background(), time.Minute)
	defer cancelSSE()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, base+"/api/events", nil)
	req.Header.Set("Authorization", bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fail("event stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail("event stream: status %d", resp.StatusCode)
	}
	type frame struct {
		at   time.Time
		data string
	}
	frames := make(chan frame, 256)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				frames <- frame{at: time.Now(), data: data}
			}
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	send := func(name, method, path string, body any) stopProbe {
		p := stopProbe{Name: name}
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("Authorization", bearer)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			p.Body = "error: " + err.Error()
			return p
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		p.Status, p.RetryAfter, p.Body = resp.StatusCode, resp.Header.Get("Retry-After"), string(raw)
		return p
	}
	newStream := func() map[string]string {
		return map[string]string{"provider": stopTestProviderID, "userMessage": "hi", "clientStreamId": uuid.NewString()}
	}

	// A chat stream, answering on the event stream: its first chunk has arrived.
	body := newStream()
	streamID := body["clientStreamId"]
	if p := send("POST /api/chat/stream", http.MethodPost, "/api/chat/stream", body); p.Status != http.StatusOK {
		return fail("start the chat stream: %d %s", p.Status, p.Body)
	}
	for live := false; !live; {
		select {
		case f, ok := <-frames:
			if !ok {
				return fail("the event stream closed before the stream's first chunk")
			}
			live = strings.Contains(f.data, streamID) && strings.Contains(f.data, `"type":"chunk"`)
		case <-time.After(20 * time.Second):
			return fail("no chunk of the chat stream on the event stream")
		}
	}

	began := time.Now()
	stopping = true
	go func() { stopped <- app.Stop(stopCtx) }()

	// While the stop drains: the probes, each timed. /readyz is polled, since the drain begins on the stop
	// goroutine; it gives up at the end of the pause, keeping the last answer.
	for deadline := began.Add(stopTestDrainDelay); ; time.Sleep(10 * time.Millisecond) {
		rep.Readyz = send("GET /readyz", http.MethodGet, "/readyz", nil)
		rep.Readyz.At = time.Since(began)
		if rep.Readyz.Status == http.StatusServiceUnavailable || time.Now().After(deadline) {
			break
		}
	}
	rep.Healthz = send("GET /healthz", http.MethodGet, "/healthz", nil)
	rep.Healthz.At = time.Since(began)
	rep.NewStream = send("POST /api/chat/stream", http.MethodPost, "/api/chat/stream", newStream())
	rep.NewStream.At = time.Since(began)

	// What the event stream delivers until it closes.
	for closed := false; !closed; {
		select {
		case f, ok := <-frames:
			if !ok {
				closed = true
				break
			}
			rep.FramesAfterStop = append(rep.FramesAfterStop, f.data)
			if rep.Terminal == "" && strings.Contains(f.data, streamID) && strings.Contains(f.data, `"type":"error"`) {
				rep.Terminal, rep.TerminalAt = f.data, f.at.Sub(began)
			}
		case <-time.After(30 * time.Second):
			return fail("the event stream did not close within 30 s of the stop")
		}
	}
	if err := <-stopped; err != nil {
		return fail("stop: %v", err)
	}
	return rep
}

// untilCancelledProvider answers one chunk, then holds the stream until its context ends: a stream still answering
// when the app stops.
type untilCancelledProvider struct{ *testutil.FakeProvider }

func (untilCancelledProvider) Stream(ctx context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	onChunk(ai.Chunk{Text: "partial…"})
	<-ctx.Done()
	return ctx.Err()
}

// stopTestKeys is a key store holding a credential for the test provider only.
type stopTestKeys struct{}

func (stopTestKeys) Save(string, string, string) error { return nil }
func (stopTestKeys) Get(_, provider string) (string, error) {
	if provider == stopTestProviderID {
		return "test-credential", nil
	}
	return "", nil
}
func (stopTestKeys) Has(_, provider string) (bool, error) { return provider == stopTestProviderID, nil }
func (stopTestKeys) Delete(string, string) error          { return nil }
