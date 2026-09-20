package service

import (
	"context"
	"sync"
	"testing"

	"pad-core/models"

	"pad-analyzer/internal/errreport"
)

// captureSink records what errreport forwarded.
type captureSink struct {
	mu    sync.Mutex
	errs  int
	attrs errreport.Attrs
	msg   string
}

func (c *captureSink) CaptureError(_ context.Context, err error, attrs errreport.Attrs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs++
	c.attrs = attrs
	if err != nil {
		c.msg = err.Error()
	}
}

func (c *captureSink) CapturePanic(_ context.Context, _ any, _ []byte, _ errreport.Attrs) {}

// TestLogError_ReportsThroughErrReport: client-side errors were only written to
// the log, so the one place that owns exception aggregation never saw the
// largest source of exceptions — they reached neither
// pad_errors_reported_total nor a registered crash-triage sink.
func TestLogError_ReportsThroughErrReport(t *testing.T) {
	sink := &captureSink{}
	errreport.Register(sink)
	t.Cleanup(func() { errreport.Register(nil) })

	svc := &SystemService{}
	svc.LogError(context.Background(), models.FrontendError{
		Message:        "TypeError: x is not a function",
		Stack:          "at render (App.tsx:10)",
		ComponentStack: "in FindingsTab",
		URL:            "http://localhost/#/flow",
	})

	if sink.errs != 1 {
		t.Fatalf("expected the browser error to be reported once, got %d", sink.errs)
	}
	if sink.msg != "TypeError: x is not a function" {
		t.Errorf("reported message = %q", sink.msg)
	}
	for _, key := range []string{"stack", "componentStack", "url"} {
		if _, ok := sink.attrs[key]; !ok {
			t.Errorf("attrs should carry %q for triage, got %v", key, sink.attrs)
		}
	}
}
