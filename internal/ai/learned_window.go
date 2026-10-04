package ai

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// Learned context windows (alias providers only): a deployment name says nothing about the model's window, so an
// uncatalogued small-window model lists at the provider-wide 128k default until its own context-length rejection
// ("maximum context length is 16384 tokens" → *ContextLimitError) states the real one; it is recorded and clamped onto
// every later listing for the same (scope, resource, deployment), so the next request is truncated to fit. Served-memo
// bounds: servedMemoMax entries, servedMemoTTL after the last rejection.

type learnedWindow struct {
	limit int
	seen  time.Time
}

var (
	learnedMu      sync.Mutex
	learnedWindows = map[string]learnedWindow{}
)

// recordContextWindow remembers the window a rejection of requestModel stated, keeping the smaller of two
// observations within the TTL (a model router may be routed to models with different windows); no-op for
// providers whose model ids are model names.
func recordContextWindow(scope, providerID, requestModel string, limit int) {
	requestModel = strings.TrimSpace(requestModel)
	if !aliasModelProviders[providerID] || requestModel == "" || limit <= 0 {
		return
	}
	now := time.Now()
	learnedMu.Lock()
	defer learnedMu.Unlock()
	if len(learnedWindows) >= servedMemoMax {
		for k, e := range learnedWindows {
			if now.Sub(e.seen) > servedMemoTTL {
				delete(learnedWindows, k)
			}
		}
		for k := range learnedWindows {
			if len(learnedWindows) < servedMemoMax {
				break
			}
			delete(learnedWindows, k)
		}
	}
	key := servedKey(scope, providerID, requestModel)
	if prev, ok := learnedWindows[key]; ok && now.Sub(prev.seen) <= servedMemoTTL && prev.limit < limit {
		limit = prev.limit
	}
	learnedWindows[key] = learnedWindow{limit: limit, seen: now}
}

// lookupContextWindow returns the learned window for requestModel (expired
// entries are dropped and reported as missing).
func lookupContextWindow(scope, providerID, requestModel string) (int, bool) {
	learnedMu.Lock()
	defer learnedMu.Unlock()
	key := servedKey(scope, providerID, requestModel)
	e, ok := learnedWindows[key]
	if !ok {
		return 0, false
	}
	if time.Since(e.seen) > servedMemoTTL {
		delete(learnedWindows, key)
		return 0, false
	}
	return e.limit, true
}

// forgetContextWindow drops the learned window of an alias whose served model changed (recordServedModel): the
// rejection that taught it belonged to the model the deployment pointed at before, and a sticky window would
// over-truncate a deployment re-pointed at a larger model. Lock order: callers may hold servedMu; nothing takes
// learnedMu before servedMu.
func forgetContextWindow(scope, providerID, requestModel string) {
	learnedMu.Lock()
	delete(learnedWindows, servedKey(scope, providerID, requestModel))
	learnedMu.Unlock()
}

func learnContextWindow(scope, providerID, requestModel string, err error) {
	var cle *ContextLimitError
	if errors.As(err, &cle) {
		recordContextWindow(scope, providerID, requestModel, cle.Limit)
	}
}

// applyLearnedWindows clamps each listed entry to its learned window (output cap a quarter of it, so prompt +
// completion fit once the chat service truncates the history). A rejection is more authoritative than any listing,
// so every entry is clamped — project-annotated, catalogued-name and agent entries too.
func applyLearnedWindows(scope, providerID string, models []ModelInfo) []ModelInfo {
	if !aliasModelProviders[providerID] {
		return models
	}
	for i := range models {
		lim, ok := lookupContextWindow(scope, providerID, models[i].ID)
		if !ok {
			continue
		}
		m := &models[i]
		if m.ContextLimit <= 0 || lim < m.ContextLimit {
			m.ContextLimit = lim
		}
		if outCap := lim / 4; outCap > 0 && (m.MaxOutputTokens <= 0 || m.MaxOutputTokens > outCap) {
			m.MaxOutputTokens = outCap
		}
	}
	return models
}

// resetLearnedWindows clears the memo (tests, ResetAzureFoundryCaches).
func resetLearnedWindows() {
	learnedMu.Lock()
	defer learnedMu.Unlock()
	learnedWindows = map[string]learnedWindow{}
}
