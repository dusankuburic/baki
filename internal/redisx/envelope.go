package redisx

import (
	"encoding/json"

	"github.com/prometheus/client_golang/prometheus"

	"pad-analyzer/internal/metrics"
)

// Envelope is the head every Redis payload written from the envelope on carries: {"v":1,"kind":"ws.room",...}.
// The rest of the payload sits beside it in the same object, so a reader that predates the envelope still decodes
// the fields it knows and ignores these two (every payload of the first release is a bare object).
//
// The rules, which make a rolling deploy safe (docs/adr/0029-redis-envelope-key-registry-and-health.md):
//   - additive only: a field is never renamed or retyped, a new one is added;
//   - a payload without "v" is version 0, the shape every release before the envelope wrote, and is accepted;
//   - a version or a kind the reader does not know is ignored and counted, never a panic and never an error to
//     the sender;
//   - a record that fails to decode is skipped, never deleted: only the record's own expiry removes it. Another
//     release may have written it, and a reader that deletes what it cannot read erases the other release's state.
type Envelope struct {
	V    int    `json:"v,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// Result is what Decode made of a payload.
type Result string

const (
	// ResultOK: the payload was understood and decoded.
	ResultOK Result = "ok"
	// ResultUnknown: a well-formed payload of a version or kind this release does not know; ignored.
	ResultUnknown Result = "unknown"
	// ResultBad: not JSON of the expected shape; ignored (and never deleted by the reader).
	ResultBad Result = "bad"
)

var msgsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "pad_redis_msgs_total",
		Help: "Redis payloads a reader decoded, by kind (the declared payload kind the reader expects, never the sender's) and result: ok, unknown (a version or kind this release does not know, ignored) or bad (not decodable, ignored). A steady unknown rate during a rolling deploy is the other release's newer payloads; a bad rate is a producer bug.",
	},
	[]string{"kind", "result"},
)

func init() { metrics.MustRegister(msgsTotal) }

// Decode reads one payload of kind into dst and counts the outcome. kind is the payload kind the caller reads
// ("ws.room"); maxV is the newest version it understands. Version 0 (no "v") is always accepted: it is the shape of
// the release before the envelope, and a sender that predates it cannot say which kind it sends, so an absent kind
// means the kind the channel carries. A "kind" that differs, or a "v" above maxV, is ResultUnknown and dst is left
// untouched. dst may be nil when the caller only wants the verdict.
func Decode(kind string, maxV int, payload []byte, dst any) Result {
	res := decode(kind, maxV, payload, dst)
	msgsTotal.WithLabelValues(kind, string(res)).Inc()
	return res
}

func decode(kind string, maxV int, payload []byte, dst any) Result {
	var head Envelope
	if err := json.Unmarshal(payload, &head); err != nil {
		return ResultBad
	}
	if head.V < 0 || head.V > maxV || (head.Kind != "" && head.Kind != kind) {
		return ResultUnknown
	}
	if dst == nil {
		return ResultOK
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return ResultBad
	}
	return ResultOK
}
