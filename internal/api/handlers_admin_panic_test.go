package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
)

// TestScannerScan_PanicInJobIsRecovered: the admin "scan now" trigger runs the
// job on a detached goroutine. The scheduled scanner loop recovers its own
// panics; this path did not, so a panic the nightly run would have logged and
// survived took the whole process down when an admin pressed the button.
//
// The assertion is the panic counter, not "the test is still alive": a
// goroutine panic kills the binary only after its deferred funcs run, so a
// channel signal alone could race the crash. pad_panics_total is incremented
// by the recover path and nothing else.
func TestScannerScan_PanicInJobIsRecovered(t *testing.T) {
	rt := newJWTTestRouter(t)
	moduleOf[*AdminHandler](t, rt).scanNow = func(context.Context) {
		panic("scanner exploded")
	}
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	labels := map[string]string{"location": "admin.scan-now"}
	before := counterSum(t, "pad_panics_total", labels)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/admin/scanner/scan", bearer, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterSum(t, "pad_panics_total", labels) > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job's panic was not recovered and counted at pad_panics_total{location=\"admin.scan-now\"}")
}
