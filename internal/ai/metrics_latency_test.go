package ai

// R-observability-8: the AI call latency is measured in buckets that reach the stream's 10-minute wall clock, by
// call kind, and a stream's time to its first content is its own histogram.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func histogramOf(t *testing.T, o prometheus.Observer) *dto.Histogram {
	t.Helper()
	var m dto.Metric
	if err := o.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram()
}

func TestAICallDuration_BucketsReachTheWallClock(t *testing.T) {
	series := aiCallDuration.WithLabelValues("latency-test", aiCallStream)
	bucket := func(h *dto.Histogram, upper float64) uint64 {
		for _, b := range h.GetBucket() {
			if b.GetUpperBound() == upper {
				return b.GetCumulativeCount()
			}
		}
		t.Fatalf("no %v s bucket", upper)
		return 0
	}
	before := histogramOf(t, series)
	observeAICall("latency-test", aiCallStream, 60)
	after := histogramOf(t, series)
	if bucket(after, 60)-bucket(before, 60) != 1 || bucket(after, 30)-bucket(before, 30) != 0 {
		t.Error("a 60 s observation did not land in the 60 s bucket")
	}
	if bucket(after, 600) != after.GetSampleCount() {
		t.Errorf("600 s bucket %d of %d samples: an observation reached +Inf", bucket(after, 600), after.GetSampleCount())
	}
}

func TestAIFirstToken_ObservedOncePerContentStream(t *testing.T) {
	stub := &estimatingStreamStub{cbStreamStub{chunks: []Chunk{{Reasoning: "hm"}, {Text: "a"}, {Text: "b"}, {Done: true}}}}
	ap := &auditedProvider{inner: stub, providerID: "first-token-test"}
	before := histogramOf(t, aiFirstToken.WithLabelValues("first-token-test")).GetSampleCount()
	if err := ap.Stream(context.Background(), Request{}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	if got := histogramOf(t, aiFirstToken.WithLabelValues("first-token-test")).GetSampleCount(); got != before+1 {
		t.Errorf("first-token samples = %d, want %d (once per stream that delivers content)", got, before+1)
	}

	stub.chunks = []Chunk{{Error: ErrProviderDown}}
	_ = ap.Stream(context.Background(), Request{}, func(Chunk) {})
	if got := histogramOf(t, aiFirstToken.WithLabelValues("first-token-test")).GetSampleCount(); got != before+1 {
		t.Errorf("a stream with no content was observed (%d samples)", got)
	}
}

// estimatingStreamStub is cbStreamStub with the token estimate the audited wrapper falls back to.
type estimatingStreamStub struct{ cbStreamStub }

func (s *estimatingStreamStub) EstimateTokens(text string) int { return len(text) }
