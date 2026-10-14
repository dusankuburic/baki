package redisx

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type demoPayload struct {
	Envelope
	Name string `json:"name"`
}

func count(kind string, res Result) float64 {
	return testutil.ToFloat64(msgsTotal.WithLabelValues(kind, string(res)))
}

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		maxV    int
		want    Result
		decoded string
	}{
		{"version 0 has no v and no kind", `{"name":"a"}`, 1, ResultOK, "a"},
		{"version 1", `{"v":1,"kind":"test.demo","name":"b"}`, 1, ResultOK, "b"},
		{"version 1 without a kind", `{"v":1,"name":"c"}`, 1, ResultOK, "c"},
		{"an older writer read by a newer reader", `{"v":1,"kind":"test.demo","name":"d"}`, 2, ResultOK, "d"},
		{"a newer version is ignored", `{"v":2,"kind":"test.demo","name":"e"}`, 1, ResultUnknown, ""},
		{"another kind is ignored", `{"v":1,"kind":"test.other","name":"f"}`, 1, ResultUnknown, ""},
		{"a negative version is ignored", `{"v":-1,"name":"g"}`, 1, ResultUnknown, ""},
		{"extra fields are tolerated", `{"v":1,"kind":"test.demo","name":"h","future":{"x":1}}`, 1, ResultOK, "h"},
		{"not JSON", `not json`, 1, ResultBad, ""},
		{"a JSON array", `[1,2]`, 1, ResultBad, ""},
		{"empty", ``, 1, ResultBad, ""},
		{"a field of the wrong type", `{"v":1,"name":7}`, 1, ResultBad, ""},
		{"a version of the wrong type", `{"v":"one","name":"i"}`, 1, ResultBad, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := count("test.demo", tc.want)
			var got demoPayload
			if res := Decode("test.demo", tc.maxV, []byte(tc.payload), &got); res != tc.want {
				t.Fatalf("Decode = %q, want %q", res, tc.want)
			}
			if got.Name != tc.decoded {
				t.Errorf("decoded name = %q, want %q (an ignored payload must leave dst untouched)", got.Name, tc.decoded)
			}
			if after := count("test.demo", tc.want); after != before+1 {
				t.Errorf("pad_redis_msgs_total{test.demo,%s} went %v -> %v, want +1", tc.want, before, after)
			}
		})
	}
}

// A nil destination asks only for the verdict.
func TestDecode_NilDestination(t *testing.T) {
	if res := Decode("test.demo", 1, []byte(`{"v":1,"name":"x"}`), nil); res != ResultOK {
		t.Errorf("Decode = %q, want ok", res)
	}
	if res := Decode("test.demo", 1, []byte(`{"v":9}`), nil); res != ResultUnknown {
		t.Errorf("Decode of a newer version = %q, want unknown", res)
	}
}
