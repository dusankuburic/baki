//go:build live

package ai

import (
	"sort"
	"strings"
	"time"

	"pad-core/cache"
)

// The live smoke's window into the package (R-testing-11): live_smoke_test.go is an external test (package ai_test,
// so it can use internal/testutil's live harness), and this file — compiled only into the live test binary — hands it
// the memos the providers learn from a vendor's rejections.

// LiveRepairMemo snapshots what the OpenAI-compatible and Responses paths have learned from rejections: the
// parameters repaired per endpoint and model (learnedRepairs; Claude's sampling and count_tokens repairs too) and
// the deployments that refused streaming (streamRefusals). Each entry is named by its kind and model only — the
// endpoint half of the key is dropped, so an Azure resource host never reaches a public job summary. A new entry
// after a live call is a vendor change the fixtures do not show yet.
func LiveRepairMemo() map[string]string {
	out := map[string]string{}
	learnedRepairs.Range(func(k cache.Key, e learnedRepair) bool {
		out["repair "+memoModel(memoKeyString(k))] = strings.Join(e.params, ",")
		return true
	})
	streamRefusals.Range(func(k cache.Key, _ time.Time) bool {
		out["stream-refusal "+memoModel(memoKeyString(k))] = "streaming refused; answered through chat"
		return true
	})
	return out
}

// memoKeyString is the endpoint + deployment string a memo key was built from (memoKey).
func memoKeyString(k cache.Key) string {
	if parts := k.Parts(); len(parts) == 1 {
		return parts[0].Value()
	}
	return ""
}

// memoModel is the model half of a memo key (endpoint + NUL + model); a key without one (count_tokens is keyed by
// its URL) is reduced to its path.
func memoModel(key string) string {
	if i := strings.LastIndexByte(key, 0); i >= 0 {
		return key[i+1:]
	}
	if i := strings.Index(key, "://"); i >= 0 {
		if j := strings.IndexByte(key[i+3:], '/'); j >= 0 {
			return key[i+3+j:]
		}
	}
	return key
}

// NewLiveRepairEntries lists the entries of after that before lacks or that changed, sorted, as "name: value".
func NewLiveRepairEntries(before, after map[string]string) []string {
	var out []string
	for k, v := range after {
		if old, ok := before[k]; !ok || old != v {
			out = append(out, k+": "+v)
		}
	}
	sort.Strings(out)
	return out
}
