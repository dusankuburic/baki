package ai

import (
	"context"
	"strings"
	"sync"
	"time"
)

// aliasModelProviders lists the providers whose model IDs are ALIASES chosen by
// the user rather than model names: an Azure AI Foundry deployment called
// "my-gpt4o" (or a model-router deployment) tells the catalogue nothing about
// the model's context window or output cap. For these providers the concrete
// model each alias served last is remembered (recordServedModel) and folded
// back into the model list (enrichServedModels). They are unpriced
// (auditedProvider.pricingFor): usage rows record the tokens at $0 with the
// model each response reports, never the memo of an earlier call. Every other
// provider's IDs are the model names themselves, so nothing is memoised for
// them and their pricing never consults the served model.
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
	// varied records that the alias has been served by more than one model
	// (compared after normalizeServedModel, so a date-version bump does not
	// count) within the TTL — a model-router deployment under any name. Such
	// an alias has no single context window or output cap.
	varied bool
}

var (
	servedMu   sync.Mutex
	servedMemo = map[string]servedEntry{}
)

// servedKey scopes the memo per user AND resource: two users' Azure resources
// may both have a deployment called "chat" that points at different models,
// and one user's observation must never leak into another's model list. The
// audited wrapper passes scope = user + "\x00" + resource endpoint for alias
// providers (see servedMemoResourcer), so a new resource's same-named
// deployment never inherits the old resource's mapping.
func servedKey(scope, providerID, requestModel string) string {
	return scope + "\x00" + providerID + "\x00" + requestModel
}

// isModelRouterAlias reports whether a deployment name denotes a model router
// (the portal default "model-router", the REST sample "model-router-deployment"):
// each call may be answered by a different model, so what served the last
// call says nothing about this call's context window, output cap or price.
func isModelRouterAlias(id string) bool {
	return strings.Contains(strings.ToLower(id), "model-router")
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
	key := servedKey(scope, providerID, requestModel)
	varied := false
	if prev, ok := servedMemo[key]; ok && now.Sub(prev.seen) <= servedMemoTTL {
		changed := normalizeServedModel(prev.model) != normalizeServedModel(served)
		varied = prev.varied || changed
		// A stable alias now answered by a different model was re-pointed
		// (or is a router seen varying for the first time): the window its
		// earlier rejection stated belonged to the old model. A known router
		// keeps its learned minimum — its served model changes every call.
		if changed && !prev.varied && !isModelRouterAlias(requestModel) {
			forgetContextWindow(scope, providerID, requestModel)
		}
	}
	servedMemo[key] = servedEntry{model: served, seen: now, varied: varied}
}

// lookupServedEntry returns the memo entry for requestModel (expired entries
// are dropped and reported as missing).
func lookupServedEntry(scope, providerID, requestModel string) (servedEntry, bool) {
	servedMu.Lock()
	defer servedMu.Unlock()
	key := servedKey(scope, providerID, requestModel)
	e, ok := servedMemo[key]
	if !ok {
		return servedEntry{}, false
	}
	if time.Since(e.seen) > servedMemoTTL {
		delete(servedMemo, key)
		return servedEntry{}, false
	}
	return e, true
}

// lookupServedModelForResource returns the model most recently seen serving
// requestModel on one resource, whichever user's call observed it: a
// deployment serves the same model for every caller of its resource, so
// this is safe for decisions about the request (the Claude sampling policy)
// and never reaches a model list. Aliases observed with more than one served
// model (routers) are skipped. A linear scan of the bounded memo.
func lookupServedModelForResource(resource, providerID, requestModel string) (string, bool) {
	if resource == "" || requestModel == "" {
		return "", false
	}
	suffix := "\x00" + resource + "\x00" + providerID + "\x00" + requestModel
	now := time.Now()
	servedMu.Lock()
	defer servedMu.Unlock()
	var best servedEntry
	found := false
	for k, e := range servedMemo {
		if !strings.HasSuffix(k, suffix) || e.varied || now.Sub(e.seen) > servedMemoTTL {
			continue
		}
		if !found || e.seen.After(best.seen) {
			best, found = e, true
		}
	}
	return best.model, found
}

// lookupServedModel returns the model last seen serving requestModel.
func lookupServedModel(scope, providerID, requestModel string) (string, bool) {
	e, ok := lookupServedEntry(scope, providerID, requestModel)
	return e.model, ok
}

// resetServedModels clears the memo (tests).
func resetServedModels() {
	servedMu.Lock()
	defer servedMu.Unlock()
	servedMemo = map[string]servedEntry{}
}

// enrichServedModels fills in, for alias providers, what the memo knows about
// each entry: the served model (ServedModel, and " · <model>" appended to the
// display name) and — when that model is catalogued, only for an alias that
// has always been served by the same model — its context window and output
// cap. A model router (by name, or an alias observed with more than one
// served model) keeps the listing's conservative window: the next call may be
// routed to a smaller-window model than the last one. Entries that already
// carry a served model (learned from a deployments listing) are left alone.
func enrichServedModels(scope, providerID string, models []ModelInfo) []ModelInfo {
	if !aliasModelProviders[providerID] {
		return models
	}
	for i := range models {
		m := &models[i]
		if m.ServedModel != "" {
			continue
		}
		e, ok := lookupServedEntry(scope, providerID, m.ID)
		if !ok {
			continue
		}
		annotateServedModel(providerID, m, e.model, !e.varied && !isModelRouterAlias(m.ID))
	}
	return models
}

// applyServedModel annotates a model-list entry with the model behind it,
// limits included — for authoritative sources (the project deployments
// listing's modelName) whose mapping is fixed.
func applyServedModel(providerID string, m *ModelInfo, served string) {
	annotateServedModel(providerID, m, served, true)
}

// annotateServedModel sets the served model and the display suffix;
// withLimits also copies the catalogue context window and output cap. No
// price: alias providers are unpriced (catalogEntryFor).
func annotateServedModel(providerID string, m *ModelInfo, served string, withLimits bool) {
	m.ServedModel = served
	if entry, ok := catalogEntryFor(providerID, served); ok {
		// Claude on Azure AI Foundry uses the catalogue window too: Anthropic's
		// Foundry page gives the same windows as the Claude API (1M for
		// Opus/Sonnet 4.6+ and 5.x, 200k for the rest).
		if withLimits && entry.ContextLimit > 0 {
			m.ContextLimit = entry.ContextLimit
		}
		if withLimits && entry.MaxOutputTokens > 0 {
			m.MaxOutputTokens = entry.MaxOutputTokens
		}
	}
	if !strings.Contains(m.DisplayName, " · ") && !strings.EqualFold(m.DisplayName, served) {
		m.DisplayName = m.DisplayName + " · " + served
	}
}

// UsesModelAliases reports whether providerID's model ids are user-chosen
// aliases (Azure AI Foundry deployment names) rather than model names.
func UsesModelAliases(providerID string) bool { return aliasModelProviders[providerID] }

// ReservationPricer is implemented by the audited wrapper — the provider the
// factory hands out — and answers the rate an in-flight budget reservation
// for model is priced at (see auditedProvider.ReservationPricing). The chat
// service prefers it over its own listing lookup: the listing price of a
// router is the memo of an EARLIER routed call.
type ReservationPricer interface {
	ReservationPricing(ctx context.Context, model string) Pricing
}

// UsagePricer is implemented by the audited wrapper and answers the
// per-million rates a finished turn was billed at (see
// auditedProvider.UsagePricing), so the chat service can put the model that
// answered and its rate on the done payload without a listing refetch.
type UsagePricer interface {
	UsagePricing(ctx context.Context, model, served string) (Pricing, bool)
}

// pricingMetricModel keeps the pricing-fallback metric's model label bounded:
// alias providers' request models are user-chosen strings (deployment names),
// so they are collapsed to one "deployment" label; non-alias providers keep a
// model's own name only while the catalogue knows it — a fallback for a name
// the catalogue has never heard of (the request model is client-chosen) would
// otherwise mint one permanent counter series per unique string, the same
// cardinality trap aiUsageCostTotal and metricToolLabel already fixed. The
// real name still lives on the UsageMetric row; only the metric collapses it.
func pricingMetricModel(providerID, modelID string) string {
	if aliasModelProviders[providerID] {
		return "deployment"
	}
	if _, ok := catalogEntryFor(providerID, modelID); !ok {
		return "uncatalogued"
	}
	return modelID
}
