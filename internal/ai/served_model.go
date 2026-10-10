package ai

import (
	"context"
	"strings"
	"sync"
	"time"
)

// The providers whose model IDs are user-chosen ALIASES, not model names (a Foundry deployment called
// "my-gpt4o" tells the catalogue nothing), are those with CapModelAliases — aliasModelProviders in spec.go,
// derived at registration: the model each alias served last is memoised (recordServedModel) and folded back
// into the model list (enrichServedModels). They are unpriced.

const (
	// servedMemoMax bounds the memo (entries are tiny; the cap only matters
	// for a pathological caller that invents deployment names).
	servedMemoMax = 4096
	// servedMemoTTL is how long an observation stays valid: a deployment can be re-pointed, so a stale
	// mapping must expire even if the deployment is never called again.
	servedMemoTTL = 24 * time.Hour
)

type servedEntry struct {
	model string
	seen  time.Time
	// resource is the scope's resource segment — the servedByResource bucket the key belongs to, kept so
	// eviction can unindex an entry without re-deriving it.
	resource string
	// varied records that the alias has been served by more than one model (compared after
	// normalizeServedModel, so a date-version bump does not count) within the TTL — a model-router: no
	// single context window or output cap.
	varied bool
}

var (
	servedMu   sync.Mutex
	servedMemo = map[string]servedEntry{}
	// servedByResource indexes the memo, resource → the full keys of every entry whose scope ends in that
	// resource: lookups scan only this bucket, exact by verifying the full suffix per key. Maintained in
	// lockstep with servedMemo (indexServedKeyLocked / deleteServedKeyLocked).
	servedByResource = map[string]map[string]struct{}{}
)

// servedKey scopes the memo per user AND resource: two users' resources may both have a deployment called
// "chat" pointing at different models, and one user's observation must never leak into another's model list.
func servedKey(scope, providerID, requestModel string) string {
	return scope + "\x00" + providerID + "\x00" + requestModel
}

// servedResourceOf returns the resource a scope is keyed to: the segment after its last separator (a scope
// with no separator is its own resource).
func servedResourceOf(scope string) string {
	if i := strings.LastIndex(scope, "\x00"); i >= 0 {
		return scope[i+1:]
	}
	return scope
}

// indexServedKeyLocked files key under res in servedByResource. Called
// with servedMu held, for every key that enters servedMemo.
func indexServedKeyLocked(key, res string) {
	bucket := servedByResource[res]
	if bucket == nil {
		bucket = map[string]struct{}{}
		servedByResource[res] = bucket
	}
	bucket[key] = struct{}{}
}

// deleteServedKeyLocked drops key from servedMemo AND its servedByResource bucket (whose emptied remains go
// too — the index must never outgrow the memo). Called with servedMu held.
func deleteServedKeyLocked(key string) {
	e, ok := servedMemo[key]
	if !ok {
		return
	}
	delete(servedMemo, key)
	if bucket := servedByResource[e.resource]; bucket != nil {
		delete(bucket, key)
		if len(bucket) == 0 {
			delete(servedByResource, e.resource)
		}
	}
}

// isModelRouterAlias reports whether a deployment name denotes a model router ("model-router",
// "model-router-deployment"): each call may be answered by a different model, so the last call's model says
// nothing about this call's window, cap or price.
func isModelRouterAlias(id string) bool {
	return strings.Contains(strings.ToLower(id), "model-router")
}

// recordServedModel remembers that requestModel (an alias) was served by served; no-op for providers whose
// IDs are model names, for empty values, and when the alias already IS the model.
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
				deleteServedKeyLocked(k)
			}
		}
		// Still full after expiring: drop entries until there is room — random map order, acceptable for a
		// cache this cheap to repopulate (one chat turn).
		for k := range servedMemo {
			if len(servedMemo) < servedMemoMax {
				break
			}
			deleteServedKeyLocked(k)
		}
	}
	key := servedKey(scope, providerID, requestModel)
	varied := false
	if prev, ok := servedMemo[key]; ok && now.Sub(prev.seen) <= servedMemoTTL {
		changed := normalizeServedModel(prev.model) != normalizeServedModel(served)
		varied = prev.varied || changed
		// A stable alias now answered by a different model was re-pointed: the window its earlier rejection
		// stated belonged to the old model. A known router keeps its learned minimum.
		if changed && !prev.varied && !isModelRouterAlias(requestModel) {
			forgetContextWindow(scope, providerID, requestModel)
		}
	}
	res := servedResourceOf(scope)
	servedMemo[key] = servedEntry{model: served, seen: now, varied: varied, resource: res}
	indexServedKeyLocked(key, res)
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
		deleteServedKeyLocked(key)
		return servedEntry{}, false
	}
	return e, true
}

// lookupServedModelForResource returns the model most recently seen serving requestModel on one resource,
// whichever user's call observed it (a deployment serves the same model for every caller of its resource);
// safe for decisions about the request, never reaches a model list. Varied aliases (routers) are skipped.
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
	for k := range servedByResource[resource] {
		e := servedMemo[k] // a missing key reads as long-expired and skips
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
	servedByResource = map[string]map[string]struct{}{}
}

// enrichServedModels fills in what the memo knows about each alias entry: the served model (ServedModel plus
// the " · <model>" display suffix) and — when that model is catalogued, only for an alias always served by
// the same model — its window and output cap. A router keeps the listing's conservative window.
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

// applyServedModel annotates a model-list entry with the model behind it, limits included — for authoritative
// sources (the project listing's modelName) whose mapping is fixed.
func applyServedModel(providerID string, m *ModelInfo, served string) {
	annotateServedModel(providerID, m, served, true)
}

// annotateServedModel sets the served model and the display suffix; withLimits also copies the catalogue
// window and output cap. No price: alias providers are unpriced.
func annotateServedModel(providerID string, m *ModelInfo, served string, withLimits bool) {
	m.ServedModel = served
	if entry, ok := catalogEntryFor(providerID, served); ok {
		// Claude on Foundry uses the catalogue window too: Anthropic's Foundry page gives the same windows
		// as the Claude API (1M for Opus/Sonnet 4.6+ and 5.x, 200k for the rest).
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
// aliases (Azure AI Foundry deployment names) rather than model names (CapModelAliases).
func UsesModelAliases(providerID string) bool { return aliasModelProviders[providerID] }

// ReservationPricer is implemented by the audited wrapper and answers the rate an in-flight budget
// reservation for model is priced at. The chat service does not consult it today: it reserves through its own
// inFlightPricing (the provider-wide rate, nothing for an alias provider), which answers the same; the
// interface stays for a caller that needs the wrapper's resolution (a router's listing price is the memo of
// an EARLIER routed call).
type ReservationPricer interface {
	ReservationPricing(ctx context.Context, model string) Pricing
}

// UsagePricer is implemented by the audited wrapper and answers the rates a finished turn was billed at, so
// the chat service can put the answering model and its rate on the done payload without a listing refetch.
type UsagePricer interface {
	UsagePricing(ctx context.Context, model, served string) (Pricing, bool)
}

// pricingMetricModel keeps the pricing-fallback metric's model label bounded: aliases collapse to one
// "deployment" label, non-alias providers keep a catalogued model's name else "uncatalogued" — a fallback
// for an unknown client-chosen name would mint one permanent series per unique string.
func pricingMetricModel(providerID, modelID string) string {
	if aliasModelProviders[providerID] {
		return "deployment"
	}
	if _, ok := catalogEntryFor(providerID, modelID); !ok {
		return "uncatalogued"
	}
	return modelID
}
