package service

import "testing"

// TestClampFixBatchLimit bounds a caller-supplied fix limit at both ends.
//
// The fix loop re-parses and re-analyses the entire document once per applied
// fix, so its cost is O(limit x document): measured linear at 294 ms / 528 ms /
// 1.06 s / 1.96 s / 3.75 s for limits of 25 / 50 / 100 / 200 / 400 on a 15 KB
// flow, and ~350 ms per fix on a 1 MB one.
//
// limit is decoded straight from the request body by handleApplyFixBatch, and
// only the LOW side was clamped — `if limit <= 0 { limit = 100 }`, under a
// comment calling 100 a "safety cap". Nothing enforced it as a maximum, so
// {"limit": 100000} against a 1 MB flow bought roughly 9.7 hours of CPU on one
// request. The server sets no write timeout and the loop takes no context, so
// neither a client disconnect nor a deadline stops it; for folder flows the
// limit is applied per member file, multiplying again.
func TestClampFixBatchLimit(t *testing.T) {
	cases := []struct {
		name  string
		given int
		want  int
	}{
		{"zero takes the default", 0, defaultFixBatchLimit},
		{"negative takes the default", -1, defaultFixBatchLimit},
		{"a sane value passes through", 25, 25},
		{"the maximum passes through", maxFixBatchLimit, maxFixBatchLimit},
		{"one over is clamped", maxFixBatchLimit + 1, maxFixBatchLimit},
		{"an absurd value is clamped, not honoured", 100000, maxFixBatchLimit},
		{"MaxInt is clamped", int(^uint(0) >> 1), maxFixBatchLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampFixBatchLimit(tc.given); got != tc.want {
				t.Errorf("clampFixBatchLimit(%d) = %d, want %d", tc.given, got, tc.want)
			}
		})
	}
}

// TestClampFixBatchLimit_BoundsWorstCaseWork states the property the constant
// exists for, so raising it is a deliberate decision about request cost rather
// than an unnoticed edit: whatever a caller asks for, the loop may not be asked
// to do more than maxFixBatchLimit units of O(document) work.
func TestClampFixBatchLimit_BoundsWorstCaseWork(t *testing.T) {
	for _, absurd := range []int{1_000, 100_000, 10_000_000} {
		if got := clampFixBatchLimit(absurd); got > maxFixBatchLimit {
			t.Errorf("a caller asking for %d got %d — the work a single request can "+
				"demand is unbounded", absurd, got)
		}
	}
}
