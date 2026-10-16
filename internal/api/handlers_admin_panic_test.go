package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/lifecycle"
)

// testAdminScanPanicJob is the job whose run panics below.
var testAdminScanPanicJob = lifecycle.DeclareJob(lifecycle.JobDecl{Name: "test_admin_scan_panic", Schedule: "tests only"})

// TestScannerScan_PanicInJobIsRecovered: the admin "scan now" starts the sweep in the background. The scheduled
// scanner loop recovers its own panics; the manual trigger once did not, so a panic the nightly run would have logged
// and survived took the whole process down when an admin pressed the button. The trigger now starts the scanner's
// job (scanner.ScanNow, lifecycle.Singleton.RunNow), whose run recovers like the scheduled one's.
//
// The assertion is the panic counter, not "the test is still alive": a
// goroutine panic kills the binary only after its deferred funcs run, so a
// channel signal alone could race the crash. pad_panics_total is incremented
// by the recover path and nothing else.
func TestScannerScan_PanicInJobIsRecovered(t *testing.T) {
	job := lifecycle.NewSingleton(lifecycle.Job{Spec: testAdminScanPanicJob, Run: func(context.Context) error {
		panic("scanner exploded")
	}}, nil, nil)
	t.Cleanup(job.Stop)
	rt := newJWTTestRouter(t)
	moduleOf[*AdminHandler](t, rt).scanNow = job.RunNow
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	labels := map[string]string{"location": "job." + testAdminScanPanicJob.Name()}
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
	t.Fatalf("the job's panic was not recovered and counted at pad_panics_total%v", labels)
}
