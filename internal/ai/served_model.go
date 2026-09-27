package ai

import (
	"strings"
	"sync"
	"time"
)

// aliasModelProviders lists the providers whose model IDs are ALIASES chosen by
// the user rather than model names: an Azure AI Foundry deployment called
// "my-gpt4o" (or a model-router deployment) tells the catalogue nothing about
// pricing or context window. For these providers the concrete model each
// alias served last is remembered (recordServedModel) and folded back into
// the model list (enrichServedModels) and into pricing (auditedProvider.record).
// Every other provider's IDs are the model names themselves, so nothing is
// memoised for them.
var aliasModelProviders = map[string]bool{
	"azure-foundry": true,
}

const (
	// servedMemoMax bounds the memo (entries are tiny; the cap only matters
	// for a pathological caller that invents deployment names).
	servedMemoMax = 4096
	// servedMemoTTL is how long a served-model observation stays valid: a
	// deployment can be re-pointed at another model, so a stale mapping must
	// eventually expire even if the deployment is never called again.
	servedMemoTTL = 24 * time.Hour
)

type servedEntry struct {
	model string
	seen  time.Time
}

var (
	servedMu   sync.Mutex
	servedMemo = map[string]servedEntry{}
)

// servedKey scopes the memo per user: two users' Azure resources may both
// have a deployment called "chat" that points at different models, and one
// user's observation must never leak into another's model list.
func servedKey(scope, providerID, requestModel string) string {
	return scope + "\x00" + providerID + "\x00" + requestModel
}

// recordServedModel remembers that requestModel (an alias) was served by
// served. No-op for providers whose IDs are model names, for empty values, and
// when the alias already IS the model.
func recordServedModel(scope, providerID, requestModel, served string) {
	if !aliasModelProviders[providerID] {
		return
	}
	served = strings.TrimSpace(served)
	requestModel = strings.TrimSpace(requestModel)
	if served == "" || requestModel == "" || strings.EqualFold(served, requestModel) {
		return
	}
	now := time.Now()
	servedMu.Lock()
	defer servedMu.Unlock()
	if len(servedMemo) >= servedMemoMax {
		for k, e := range servedMemo {
			if now.Sub(e.seen) > servedMemoTTL {
				delete(servedMemo, k)
			}
		}
		// Still full after expiring: drop entries until there is room. Map
		// iteration order is random, which is an acceptable eviction policy
		// for a cache this cheap to repopulate (one chat turn).
		for k := range servedMemo {
			if len(servedMemo) < servedMemoMax {
				break
			}
			delete(servedMemo, k)
		}
	}
	servedMemo[servedKey(scope, providerID, requestModel)] = servedEntry{model: served, seen: now}
}

// lookupServedModel returns the model last seen serving requestModel.
func lookupServedModel(scope, providerID, requestModel string) (string, bool) {
	servedMu.Lock()
	defer servedMu.Unlock()
	e, ok := servedMemo[servedKey(scope, providerID, requestModel)]
	if !ok {
		return "", false
	}
	if time.Since(e.seen) > servedMemoTTL {
		delete(servedMemo, servedKey(scope, providerID, requestModel))
		return "", false
	}
	return e.model, true
}

// resetServedModels clears the memo (tests).
func resetServedModels() {
	servedMu.Lock()
	defer servedMu.Unlock()
	servedMemo = map[string]servedEntry{}
}

// enrichServedModels fills in, for alias providers, what the memo knows about
// each entry: the served model (ServedModel, and " · <model>" appended to the
// display name) and, when that model is catalogued, its pricing, context
// window and output cap. Entries that already carry pricing or a served model
// (learned from a deployments listing) are left alone.
func enrichServedModels(scope, providerID string, models []ModelInfo) []ModelInfo {
	if !aliasModelProviders[providerID] {
		return models
	}
	for i := range models {
		m := &models[i]
		if m.ServedModel != "" || m.Pricing != (Pricing{}) {
			continue
		}
		served, ok := lookupServedModel(scope, providerID, m.ID)
		if !ok {
			continue
		}
		applyServedModel(providerID, m, served)
	}
	return models
}

// applyServedModel annotates a model-list entry with the model behind it.
func applyServedModel(providerID string, m *ModelInfo, served string) {
	m.ServedModel = served
	if entry, ok := catalogEntryFor(providerID, served); ok {
		m.Pricing = entry.Pricing
		if entry.ContextLimit > 0 {
			m.ContextLimit = entry.ContextLimit
		}
		if entry.MaxOutputTokens > 0 {
			m.MaxOutputTokens = entry.MaxOutputTokens
		}
	}
	if !strings.Contains(m.DisplayName, " · ") && !strings.EqualFold(m.DisplayName, served) {
		m.DisplayName = m.DisplayName + " · " + served
	}
}

// pricingMetricModel keeps the pricing-fallback metric's model label bounded:
// alias providers' request models are user-chosen strings (deployment
// names), so they are collapsed to one label instead of minting a Prometheus
// series per name.
func pricingMetricModel(providerID, modelID string) string {
	if aliasModelProviders[providerID] {
		return "deployment"
	}
	return modelID
}
