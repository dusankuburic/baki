package metrics

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var updateMetrics = flag.Bool("update-metrics", false, "rewrite testdata/metrics.golden")

// family is one metric family as the golden records it.
type family struct {
	name, kind, labels, pkg, help string
}

// descPattern parses prometheus.Desc.String(), the one public view of a descriptor's name, help and labels.
var descPattern = regexp.MustCompile(`^Desc\{fqName: ("(?:[^"\\]|\\.)*"), help: ("(?:[^"\\]|\\.)*"), constLabels: \{(.*)\}, variableLabels: \{(.*)\}\}$`)

func parseDesc(t *testing.T, d *prometheus.Desc) (name, help, labels string) {
	t.Helper()
	m := descPattern.FindStringSubmatch(d.String())
	if m == nil {
		t.Fatalf("cannot parse %s: the client library changed Desc.String; adapt descPattern", d)
	}
	name, err := strconv.Unquote(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if help, err = strconv.Unquote(m[2]); err != nil {
		t.Fatal(err)
	}
	labels = strings.ReplaceAll(m[4], ",", ", ")
	if m[3] != "" {
		if labels != "" {
			labels += "; "
		}
		labels += "const " + m[3]
	}
	if labels == "" {
		labels = "-"
	}
	return name, help, labels
}

// variableLabels returns the names of d's variable labels, in order.
func variableLabels(t *testing.T, d *prometheus.Desc) []string {
	t.Helper()
	m := descPattern.FindStringSubmatch(d.String())
	if m == nil {
		t.Fatalf("cannot parse %s: the client library changed Desc.String; adapt descPattern", d)
	}
	if m[4] == "" {
		return nil
	}
	return strings.Split(m[4], ",")
}

func describe(c prometheus.Collector) []*prometheus.Desc {
	ch := make(chan *prometheus.Desc)
	go func() { c.Describe(ch); close(ch) }()
	var out []*prometheus.Desc
	for d := range ch {
		out = append(out, d)
	}
	return out
}

// kinds maps each descriptor of c (by its String) to its metric type. A vector's type is its Go type: it emits
// nothing until a label set is used. Any other collector is collected once and its samples say.
func kinds(t *testing.T, c prometheus.Collector) map[string]string {
	t.Helper()
	vec := ""
	switch c.(type) {
	case *prometheus.CounterVec:
		vec = "counter"
	case *prometheus.GaugeVec:
		vec = "gauge"
	case *prometheus.HistogramVec:
		vec = "histogram"
	case *prometheus.SummaryVec:
		vec = "summary"
	}
	out := map[string]string{}
	if vec != "" {
		for _, d := range describe(c) {
			out[d.String()] = vec
		}
		return out
	}
	ch := make(chan prometheus.Metric)
	go func() { c.Collect(ch); close(ch) }()
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write %s: %v", m.Desc(), err)
		}
		kind := "untyped"
		switch {
		case pb.Counter != nil:
			kind = "counter"
		case pb.Gauge != nil:
			kind = "gauge"
		case pb.Histogram != nil:
			kind = "histogram"
		case pb.Summary != nil:
			kind = "summary"
		}
		out[m.Desc().String()] = kind
	}
	return out
}

// registeredFamilies lists every family on the registry with the package that registered it. The client library's
// Go runtime and process collectors are one line each: their families are the library's and follow the Go version
// and the OS.
func registeredFamilies(t *testing.T) []family {
	t.Helper()
	var out []family
	for _, reg := range registry.registrations() {
		pkg := strings.TrimPrefix(reg.pkg, modulePath+"/")
		descs := describe(reg.collector)
		if prefix := commonPrefix(t, descs, "go_", "process_"); prefix != "" {
			out = append(out, family{name: prefix + "*", kind: "runtime collector", labels: "-", pkg: pkg,
				help: "the client library's " + strings.TrimSuffix(prefix, "_") + " collector; its families follow the Go version and the OS"})
			continue
		}
		types := kinds(t, reg.collector)
		for _, d := range descs {
			name, help, labels := parseDesc(t, d)
			kind, ok := types[d.String()]
			if !ok {
				t.Fatalf("%s: its collector emitted no sample, so its type is unknown", name)
			}
			out = append(out, family{name, kind, labels, pkg, help})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// commonPrefix returns the one of prefixes every descriptor's name starts with, or "".
func commonPrefix(t *testing.T, descs []*prometheus.Desc, prefixes ...string) string {
	t.Helper()
	for _, p := range prefixes {
		all := len(descs) > 0
		for _, d := range descs {
			if name, _, _ := parseDesc(t, d); !strings.HasPrefix(name, p) {
				all = false
			}
		}
		if all {
			return p
		}
	}
	return ""
}

// TestMetricsGolden records every metric family /metrics serves: name, type, labels, the package that registered it
// and the help text. Dashboards, alert rules and the KQL recipes name these, so a rename or a label change is a
// reviewed diff. Refresh only for an intended change:
// go test ./internal/metrics/ -run TestMetricsGolden -update-metrics
func TestMetricsGolden(t *testing.T) {
	var b strings.Builder
	seen := map[string]bool{}
	for _, f := range registeredFamilies(t) {
		if seen[f.name] {
			t.Errorf("%s is registered twice", f.name)
		}
		seen[f.name] = true
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s\n", f.name, f.kind, f.labels, f.pkg, f.help)
	}
	got := b.String()
	path := filepath.Join("testdata", "metrics.golden")
	if *updateMetrics {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (create it with -update-metrics)", err)
	}
	if want := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != want {
		t.Errorf("registered metrics changed; review, then refresh with -update-metrics.\n--- golden\n%s--- now\n%s", want, got)
	}
}

// TestMetrics_HistogramBuckets pins the buckets of every histogram, which the golden cannot show (a descriptor carries
// none): a bucket change breaks every quantile a dashboard computes over a range that spans it. A new histogram is
// listed here.
func TestMetrics_HistogramBuckets(t *testing.T) {
	defBuckets := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	want := map[string][]float64{
		"ai_request_duration_seconds":           defBuckets,
		"http_request_duration_seconds":         defBuckets,
		"pad_background_task_duration_seconds":  {0.01, 0.05, 0.25, 1, 5, 15, 60, 300, 1800},
		"pad_blob_operation_duration_seconds":   defBuckets,
		"pad_event_subscriber_duration_seconds": {0.0001, 0.001, 0.01, 0.05, 0.25, 1, 5, 30},
		"pad_job_duration_seconds":              {0.1, 1, 10, 60, 300, 900, 1800, 3600, 7200},
	}
	got := map[string][]float64{}
	for _, reg := range registry.registrations() {
		if commonPrefix(t, describe(reg.collector), "go_", "process_") != "" {
			continue // the client library's
		}
		for name, b := range histogramBuckets(t, reg.collector) {
			got[name] = b
		}
	}
	for name, b := range got {
		if w, ok := want[name]; !ok {
			t.Errorf("histogram %s has buckets %v: list them in this test", name, b)
		} else if !slices.Equal(b, w) {
			t.Errorf("histogram %s has buckets %v, want %v", name, b, w)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("histogram %s is not registered", name)
		}
	}
}

// histogramBuckets returns the bucket upper bounds of each histogram c exports, by family name. A vector has no
// series to read them from until a label set is used, so it gets one for the read and drops it again.
func histogramBuckets(t *testing.T, c prometheus.Collector) map[string][]float64 {
	t.Helper()
	out := map[string][]float64{}
	read := func(m prometheus.Metric) {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write %s: %v", m.Desc(), err)
		}
		if pb.Histogram == nil {
			return
		}
		name, _, _ := parseDesc(t, m.Desc())
		var bounds []float64
		for _, b := range pb.Histogram.GetBucket() {
			bounds = append(bounds, b.GetUpperBound())
		}
		out[name] = bounds
	}
	if vec, ok := c.(*prometheus.HistogramVec); ok {
		for _, d := range describe(c) {
			probe := variableLabels(t, d)
			for i := range probe {
				probe[i] = "bucket-probe"
			}
			read(vec.WithLabelValues(probe...).(prometheus.Metric))
			vec.DeleteLabelValues(probe...)
		}
		return out
	}
	ch := make(chan prometheus.Metric)
	go func() { c.Collect(ch); close(ch) }()
	for m := range ch {
		read(m)
	}
	return out
}

// modulePath is the root module's path, trimmed from the packages in the golden.
var modulePath = strings.TrimSuffix(thisPackage, "/internal/metrics")
