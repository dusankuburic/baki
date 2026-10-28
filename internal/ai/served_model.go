package ai

import (
	"strings"
	"sync"
	"time"

	"pad-core/cache"
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
	// varied records that the alias has been served by more than one model (compared after
	// normalizeServedModel, so a date-version bump does not count) within the TTL — a model-router: no
	// single context window or output cap.
	varied bool
}

// The served-model memo (R-provider-31): declared caches, least recently used first at servedMemoMax. servedMemo is
// per credential scope and resource (ai.served-models); servedByResource is the resource-wide view
// (ai.served-by-resource): per resource and deployment, each caller's latest stable observation (resourceObservations),
// charged one per observation. servedMu serialises recordServedModel's read-compare-write across the two.
var (
	servedMu         sync.Mutex
	servedMemo       = cache.New[servedEntry](servedModelsSpec, servedMemoMax, cache.WithMaxEntries(servedMemoMax), cache.WithTTL(servedMemoTTL))
	servedByResource = cache.New[resourceObservations](servedByResourceSpec, servedMemoMax, cache.WithMaxEntries(servedMemoMax), cache.WithTTL(servedMemoTTL))
)

// resourceObservations are one deployment's stable observations on one resource, by the observing scope's owner:
// one caller's varied observation (a re-pointed deployment) drops that caller's alone, never another's stable one.
// Never mutated once stored: recordServedModel stores a changed copy.
type resourceObservations map[string]servedEntry

// servedResourceOwnersMax bounds one deployment's observations (a resource's callers); the oldest go first.
const servedResourceOwnersMax = 64

// servedKey scopes the memo per user AND resource: two users' resources may both have a deployment called
// "chat" pointing at different models, and one user's observation must never leak into another's model list.
func servedKey(scope, providerID, requestModel string) cache.Key {
	return scopedMemoKey(servedModelsSpec, scope, providerID, requestModel)
}

// splitServedScope splits a memo scope into its owner (the credential scope) and its resource: everything after the
// owner's separator, which for Azure AI Foundry is the endpoint AND the project (servedMemoResource), the same
// string lookupServedModelForResource is asked with. A scope with no separator is its own resource.
func splitServedScope(scope string) (owner, resource string) {
	if owner, resource, ok := strings.Cut(scope, "\x00"); ok {
		return owner, resource
	}
	return scope, scope
}

// servedResourceKey keys servedByResource: the resource, the provider and the requested deployment.
func servedResourceKey(resource, providerID, requestModel string) cache.Key {
	k, _ := servedByResourceSpec.Key(cache.Param("resource", resource), cache.Param("provider", providerID), cache.Param("model", requestModel))
	return k
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
	key := servedKey(scope, providerID, requestModel)
	varied := false
	if prev, ok := servedMemo.Get(key); ok && now.Sub(prev.seen) <= servedMemoTTL {
		changed := normalizeServedModel(prev.model) != normalizeServedModel(served)
		varied = prev.varied || changed
		// A stable alias now answered by a different model was re-pointed: the window its earlier rejection
		// stated belonged to the old model. A known router keeps its learned minimum.
		if changed && !prev.varied && !isModelRouterAlias(requestModel) {
			forgetContextWindow(scope, providerID, requestModel)
		}
	}
	e := servedEntry{model: served, seen: now, varied: varied}
	servedMemo.Set(key, e, 1, 0)
	// The resource-wide view keeps each caller's stable observation: an alias this caller saw served by more than one
	// model (a router, or a deployment re-pointed within the TTL) drops this caller's observation only.
	owner, resource := splitServedScope(scope)
	rk := servedResourceKey(resource, providerID, requestModel)
	cur, _ := servedByResource.Get(rk)
	next := make(resourceObservations, len(cur)+1)
	for o, prev := range cur {
		if now.Sub(prev.seen) <= servedMemoTTL {
			next[o] = prev
		}
	}
	if varied {
		delete(next, owner)
	} else {
		next[owner] = e
	}
	for len(next) > servedResourceOwnersMax {
		oldest := ""
		for o, prev := range next {
			if oldest == "" || prev.seen.Before(next[oldest].seen) {
				oldest = o
			}
		}
		delete(next, oldest)
	}
	if len(next) == 0 {
		servedByResource.Delete(rk)
		return
	}
	servedByResource.Set(rk, next, int64(len(next)), 0)
}

// lookupServedEntry returns the memo entry for requestModel (expired entries
// are dropped and reported as missing).
func lookupServedEntry(scope, providerID, requestModel string) (servedEntry, bool) {
	key := servedKey(scope, providerID, requestModel)
	e, ok := servedMemo.Get(key)
	if !ok {
		return servedEntry{}, false
	}
	if time.Since(e.seen) > servedMemoTTL {
		servedMemo.Delete(key)
		return servedEntry{}, false
	}
	return e, true
}

// lookupServedModelForResource returns the model most recently seen serving requestModel on one resource,
// whichever user's call observed it (a deployment serves the same model for every caller of its resource);
// safe for decisions about the request, never reaches a model list. Varied aliases (routers) are not answered.
func lookupServedModelForResource(resource, providerID, requestModel string) (string, bool) {
	if resource == "" || requestModel == "" {
		return "", false
	}
	obs, _ := servedByResource.Get(servedResourceKey(resource, providerID, requestModel))
	var best servedEntry
	found := false
	for _, e := range obs {
		if e.varied || time.Since(e.seen) > servedMemoTTL {
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
	servedMemo.Clear()
	servedByResource.Clear()
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
